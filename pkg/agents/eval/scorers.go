package eval

import (
	"context"
	"fmt"
	"strings"

	"github.com/bytedance/sonic"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
)

// Contains scores 1 when the final answer contains Expect.Text.
//
// It is the workhorse for factual cases: it asks whether the answer carries the
// thing it had to carry, without pinning the wording around it. Prefer it to
// ExactMatch for anything a model phrases freely.
type Contains struct {
	// CaseSensitive compares verbatim. Off by default.
	CaseSensitive bool
}

func (Contains) Name() string { return "contains" }

func (s Contains) Score(_ context.Context, c Case, r *Result) (Score, error) {
	if c.Expect.Text == "" {
		return Score{}, fmt.Errorf("case %q has no Expect.Text", c.Name)
	}

	got, want := r.Text, c.Expect.Text
	if !s.CaseSensitive {
		got, want = strings.ToLower(got), strings.ToLower(want)
	}
	if strings.Contains(got, want) {
		return Score{Value: 1, Reason: "found " + quote(c.Expect.Text)}, nil
	}
	return Score{Value: 0, Reason: "missing " + quote(c.Expect.Text) + "; got " + quote(truncate(r.Text, 120))}, nil
}

// ExactMatch scores 1 when the final answer equals Expect.Text, ignoring
// surrounding whitespace.
//
// Reach for it only when the output is genuinely constrained — a
// classification label, a JSON payload, an enum. On prose it measures
// phrasing, not correctness, and will report regressions that are only
// rewording.
type ExactMatch struct {
	CaseSensitive bool
}

func (ExactMatch) Name() string { return "exact_match" }

func (s ExactMatch) Score(_ context.Context, c Case, r *Result) (Score, error) {
	got, want := strings.TrimSpace(r.Text), strings.TrimSpace(c.Expect.Text)
	if !s.CaseSensitive {
		got, want = strings.ToLower(got), strings.ToLower(want)
	}
	if got == want {
		return Score{Value: 1, Reason: "exact match"}, nil
	}
	return Score{
		Value:  0,
		Reason: "want " + quote(truncate(c.Expect.Text, 80)) + ", got " + quote(truncate(r.Text, 80)),
	}, nil
}

// ToolTrajectory scores how closely the tools the agent called match
// Expect.Tools.
//
// This is the metric that catches the failures a text scorer cannot see: an
// agent that reaches the right answer by guessing rather than by looking it up
// scores full marks on its text and zero here, and that gap is usually the
// thing worth knowing.
type ToolTrajectory struct {
	// Ordered requires the calls to appear in the expected order, scoring the
	// fraction of positions that match. When false the comparison is a set
	// one — Jaccard overlap — which is the right choice when the agent is
	// free to gather information in any order.
	Ordered bool
}

func (ToolTrajectory) Name() string { return "tool_trajectory" }

func (s ToolTrajectory) Score(_ context.Context, c Case, r *Result) (Score, error) {
	want, got := c.Expect.Tools, r.Tools

	if len(want) == 0 {
		// Expecting no tools is a real expectation, not a missing one: it is
		// how a case asserts the agent answered from what it already had.
		if len(got) == 0 {
			return Score{Value: 1, Reason: "no tools expected, none called"}, nil
		}
		return Score{Value: 0, Reason: "expected no tools, called " + strings.Join(got, ", ")}, nil
	}

	if s.Ordered {
		matched := 0
		for i := range want {
			if i < len(got) && got[i] == want[i] {
				matched++
			}
		}
		value := float64(matched) / float64(max(len(want), len(got)))
		return Score{
			Value:  value,
			Reason: fmt.Sprintf("ordered %d/%d; want [%s], got [%s]", matched, len(want), strings.Join(want, " "), strings.Join(got, " ")),
		}, nil
	}

	wantSet, gotSet := toSet(want), toSet(got)
	inter := 0
	for name := range wantSet {
		if _, ok := gotSet[name]; ok {
			inter++
		}
	}
	union := len(wantSet) + len(gotSet) - inter
	value := float64(inter) / float64(union)
	return Score{
		Value:  value,
		Reason: fmt.Sprintf("overlap %d/%d; want [%s], got [%s]", inter, union, strings.Join(want, " "), strings.Join(got, " ")),
	}, nil
}

// LLMJudge asks a model to rate the answer against the reference.
//
// It is the only built-in scorer that can judge an answer that is correct but
// worded nothing like the reference, and the only one whose verdict is itself a
// model output — so treat a judge score as evidence, not ground truth, and pin
// the judge's model when you care about comparing runs over time.
type LLMJudge struct {
	// Provider is the model that judges. Required. Use a capable model: a
	// weak judge produces noise that looks like agent regression.
	Provider llm.Provider

	// Model overrides the provider's default.
	Model string

	// Rubric replaces the default criteria. State what earns a 1 and what
	// earns a 0; the scale below is fixed.
	Rubric string
}

func (LLMJudge) Name() string { return "llm_judge" }

const defaultRubric = `Score 1.0 when the answer is factually correct and addresses the request, even if it is worded differently from the reference. ` +
	`Score around 0.5 when it is partially correct, or correct but omits something the reference treats as essential. ` +
	`Score 0.0 when it is wrong, evasive, or answers a different question.`

type judgeVerdict struct {
	Score  float64 `json:"score"`
	Reason string  `json:"reason"`
}

func (s LLMJudge) Score(ctx context.Context, c Case, r *Result) (Score, error) {
	if s.Provider == nil {
		return Score{}, fmt.Errorf("LLMJudge.Provider is required")
	}

	rubric := s.Rubric
	if rubric == "" {
		rubric = defaultRubric
	}

	instruction := "You are grading one answer produced by an AI agent. " + rubric +
		` Reply with only a JSON object: {"score": <number between 0 and 1>, "reason": "<one sentence>"}.` +
		" Judge only the answer's substance. Do not reward or penalize length, tone, or formatting."

	prompt := fmt.Sprintf("Request:\n%s\n\nReference answer:\n%s\n\nAgent's answer:\n%s",
		strings.Join(c.Turns, "\n"), c.Expect.Text, r.Text)

	resp, err := s.Provider.NewResponses(ctx, &responses.Request{
		Model:        s.Model,
		Instructions: utils.Ptr(instruction),
		Input: responses.InputUnion{
			OfInputMessageList: responses.InputMessageList{userMessage(prompt)},
		},
	})
	if err != nil {
		return Score{}, fmt.Errorf("judge call: %w", err)
	}

	raw := judgeText(resp)
	verdict, err := parseVerdict(raw)
	if err != nil {
		return Score{}, fmt.Errorf("%w: %s", err, quote(truncate(raw, 200)))
	}

	// Clamp rather than reject: a judge that answers 5 on a 0-1 scale has
	// still expressed "good", and failing the case would blame the agent for
	// the judge's arithmetic.
	value := min(max(verdict.Score, 0), 1)
	return Score{Value: value, Reason: verdict.Reason}, nil
}

func judgeText(resp *responses.Response) string {
	if resp == nil {
		return ""
	}
	var b strings.Builder
	for _, msg := range resp.Output {
		if msg.OfOutputMessage == nil || msg.OfOutputMessage.Content == nil {
			continue
		}
		for _, c := range *msg.OfOutputMessage.Content {
			if c.OfOutputText != nil {
				b.WriteString(c.OfOutputText.Text)
			}
		}
	}
	return b.String()
}

// parseVerdict tolerates a judge that wraps its JSON in prose or a code fence,
// which every model does occasionally however firmly it was told not to.
func parseVerdict(raw string) (judgeVerdict, error) {
	var v judgeVerdict

	start := strings.Index(raw, "{")
	end := strings.LastIndex(raw, "}")
	if start < 0 || end < start {
		return v, fmt.Errorf("judge returned no JSON object")
	}

	if err := sonic.Unmarshal([]byte(raw[start:end+1]), &v); err != nil {
		return v, fmt.Errorf("judge returned unparseable JSON")
	}
	return v, nil
}

// ScorerFunc adapts a plain function into a Scorer, for a one-off metric that
// does not need a type.
type ScorerFunc struct {
	Label string
	Fn    func(ctx context.Context, c Case, r *Result) (Score, error)
}

func (s ScorerFunc) Name() string { return s.Label }

func (s ScorerFunc) Score(ctx context.Context, c Case, r *Result) (Score, error) {
	return s.Fn(ctx, c, r)
}

func toSet(names []string) map[string]struct{} {
	out := make(map[string]struct{}, len(names))
	for _, n := range names {
		out[n] = struct{}{}
	}
	return out
}

func quote(s string) string { return `"` + s + `"` }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
