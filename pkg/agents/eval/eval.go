// Package eval runs an agent against a fixed set of cases and scores what it
// did, so a change to a prompt, a model, or a tool can be measured instead of
// eyeballed.
//
// A Case is one scenario: the turns to send, and what the agent was supposed to
// do. A Scorer turns a completed run into a number. A Runner executes every
// case, applies every scorer, and returns a Report.
//
// Scoring is separated from running on purpose: the Runner captures what
// happened — the final text, the tools called, the per-turn outputs — and
// scorers read that record. A new metric is a new Scorer, not a change here.
package eval

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
)

// Case is one scenario to run the agent through.
type Case struct {
	// Name identifies the case in reports. Keep it unique and stable — it is
	// how a regression is traced back to a scenario.
	Name string

	// Turns are the user messages to send, in order. Each runs as its own
	// agent run on a shared thread, so the agent sees the earlier turns as
	// history. Most cases have one.
	Turns []string

	// Expect describes what should have happened. Built-in scorers read it;
	// a custom scorer may ignore it entirely and assert on Result directly.
	Expect Expect

	// Threshold overrides Runner.Threshold for this case. Use it for a
	// scenario that is legitimately harder or softer than the rest of the set.
	// Nil means the Runner's value applies.
	Threshold *float64
}

// Expect is the reference the built-in scorers compare against.
type Expect struct {
	// Text is the reference answer for the final turn.
	Text string

	// Tools is the tool-call trajectory the agent should have taken, by tool
	// name, in order. Only ToolTrajectory reads it.
	Tools []string
}

// Score is one scorer's verdict on one case.
type Score struct {
	// Value is in [0,1]. Scorers that are inherently pass/fail return 0 or 1.
	Value float64

	// Reason explains the value in one line. It is what a failing report
	// shows, so make it say what was expected and what happened.
	Reason string
}

// Scorer turns a completed run into a number. Implementations must be safe for
// concurrent use: the Runner may score several cases at once.
type Scorer interface {
	// Name identifies the scorer in reports and keys it in Result.Scores.
	Name() string

	// Score judges one result. Returning an error fails the case outright,
	// which is right for a scorer that could not reach a verdict (a judge
	// model that errored) and wrong for one that reached a bad verdict —
	// that is a zero Value, not an error.
	Score(ctx context.Context, c Case, r *Result) (Score, error)
}

// Result is the record of running one case: what the agent did, and how it
// scored.
type Result struct {
	Case Case

	// Outputs holds one entry per turn, in order.
	Outputs []*agents.AgentOutput

	// Text is the assistant's text from the final turn, flattened.
	Text string

	// Tools is every tool the agent called across all turns, in call order.
	// Duplicates are kept — calling a tool twice is different from calling it
	// once, and a trajectory scorer needs to see that.
	Tools []string

	// Err is set when the run itself failed, as opposed to scoring badly. A
	// case with Err set is always a failure, whatever the scores say.
	Err error

	// Scores is keyed by scorer name.
	Scores map[string]Score

	Duration time.Duration
}

// Mean is the average of every score, or 0 when nothing scored.
func (r *Result) Mean() float64 {
	if len(r.Scores) == 0 {
		return 0
	}
	var sum float64
	for _, s := range r.Scores {
		sum += s.Value
	}
	return sum / float64(len(r.Scores))
}

// Passed reports whether the case cleared the threshold. A run that errored
// never passes, regardless of threshold.
func (r *Result) Passed(threshold float64) bool {
	if r.Err != nil {
		return false
	}
	if r.Case.Threshold != nil {
		threshold = *r.Case.Threshold
	}
	return r.Mean() >= threshold
}

// Report is the outcome of a whole run.
type Report struct {
	Results   []*Result
	Threshold float64
	Duration  time.Duration
}

// Failures returns the cases that did not clear their threshold.
func (rep *Report) Failures() []*Result {
	var out []*Result
	for _, r := range rep.Results {
		if !r.Passed(rep.Threshold) {
			out = append(out, r)
		}
	}
	return out
}

// Passed and Failed count cases, not scores.
func (rep *Report) Passed() int { return len(rep.Results) - len(rep.Failures()) }
func (rep *Report) Failed() int { return len(rep.Failures()) }

// MeanByScorer averages each scorer across every case that it scored. This is
// the number to watch across runs: a drop in one scorer localizes a regression
// far better than a drop in the overall pass count.
func (rep *Report) MeanByScorer() map[string]float64 {
	sums := map[string]float64{}
	counts := map[string]int{}
	for _, r := range rep.Results {
		for name, s := range r.Scores {
			sums[name] += s.Value
			counts[name]++
		}
	}
	out := make(map[string]float64, len(sums))
	for name, sum := range sums {
		out[name] = sum / float64(counts[name])
	}
	return out
}

// String renders a report for a terminal or a CI log: the headline, each
// scorer's mean, and a line per failure explaining itself.
func (rep *Report) String() string {
	var b strings.Builder

	fmt.Fprintf(&b, "%d/%d passed (threshold %.2f) in %s\n",
		rep.Passed(), len(rep.Results), rep.Threshold, rep.Duration.Round(time.Millisecond))

	means := rep.MeanByScorer()
	names := make([]string, 0, len(means))
	for name := range means {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(&b, "  %-20s %.2f\n", name, means[name])
	}

	for _, r := range rep.Failures() {
		if r.Err != nil {
			fmt.Fprintf(&b, "\nFAIL %s: %v\n", r.Case.Name, r.Err)
			continue
		}
		fmt.Fprintf(&b, "\nFAIL %s (mean %.2f)\n", r.Case.Name, r.Mean())
		for _, name := range names {
			if s, ok := r.Scores[name]; ok {
				fmt.Fprintf(&b, "  %-20s %.2f  %s\n", name, s.Value, s.Reason)
			}
		}
	}

	return b.String()
}

// userMessage builds the provider message for one turn of a case.
func userMessage(text string) responses.InputMessageUnion {
	return responses.InputMessageUnion{
		OfInputMessage: &responses.InputMessage{
			Role:    constants.RoleUser,
			Content: responses.InputContent{{OfInputText: &responses.InputTextContent{Text: text}}},
		},
	}
}

// extract pulls the assistant text and the tool-call names out of one run's
// output. Both message shapes are handled: the loop converts provider output
// through AsInput, which keeps assistant turns as output messages, but a
// synthetic turn (a cancellation notice, say) arrives as an input message
// roled assistant.
func extract(out *agents.AgentOutput) (text string, tools []string) {
	if out == nil {
		return "", nil
	}

	var b strings.Builder
	for _, msg := range out.Output {
		switch {
		case msg.OfFunctionCall != nil:
			tools = append(tools, msg.OfFunctionCall.Name)

		case msg.OfOutputMessage != nil && msg.OfOutputMessage.Content != nil:
			for _, c := range *msg.OfOutputMessage.Content {
				if c.OfOutputText != nil {
					b.WriteString(c.OfOutputText.Text)
				}
			}

		case msg.OfInputMessage != nil && msg.OfInputMessage.Role == constants.RoleAssistant:
			for _, c := range msg.OfInputMessage.Content {
				if c.OfInputText != nil {
					b.WriteString(c.OfInputText.Text)
				}
				if c.OfOutputText != nil {
					b.WriteString(c.OfOutputText.Text)
				}
			}
		}
	}

	return b.String(), tools
}
