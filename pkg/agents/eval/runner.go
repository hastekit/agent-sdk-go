package eval

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/messages"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
)

// AgentFor supplies the agent a case runs against. It is called once per case.
//
// It is a factory rather than a single agent because Agent carries per-run
// state — the durable-step latch among it — that is shared by the copy WithLLM
// returns. Handing every case its own agent keeps concurrent cases from
// colliding, and lets a case vary the configuration it is measuring: two cases
// can run the same scenario against different models and be compared directly.
type AgentFor func(ctx context.Context, c Case) (*agents.Agent, error)

// Fixed returns an AgentFor that hands every case the same agent. Use it when
// the agent is safe to share — a read-only agent with no runtime, or a Runner
// left at the default Concurrency of 1.
func Fixed(a *agents.Agent) AgentFor {
	return func(context.Context, Case) (*agents.Agent, error) { return a, nil }
}

// Runner executes cases and scores them.
type Runner struct {
	// Agent supplies the agent under test. Required.
	Agent AgentFor

	// Scorers are applied to every case. A case with no scorers records a
	// run but scores 0, which fails any threshold above zero — that is
	// deliberate, since a suite that silently scores nothing is worse than
	// one that fails loudly.
	Scorers []Scorer

	// Threshold is the mean score a case must reach to pass. The zero value
	// passes everything that ran without erroring, which is only useful for
	// collecting a baseline; set it once you know what good looks like.
	Threshold float64

	// Concurrency caps how many cases run at once. Zero or one runs them
	// sequentially. Raising it needs an Agent factory that returns a distinct
	// agent per case, and tools that tolerate concurrent use.
	Concurrency int

	// Namespace is the persistence namespace for eval threads. Defaults to
	// "eval".
	Namespace string
}

// Run executes every case and returns the report. It does not stop at the
// first failure: the point is the whole picture, and a suite that aborts early
// hides the rest of the regressions.
//
// The error return is for a Runner that cannot run at all, not for a case that
// failed. A failing case is a Result with Err set.
func (r *Runner) Run(ctx context.Context, cases []Case) (*Report, error) {
	if r.Agent == nil {
		return nil, fmt.Errorf("eval: Runner.Agent is required")
	}
	if len(cases) == 0 {
		return nil, fmt.Errorf("eval: no cases to run")
	}
	if err := r.checkNames(cases); err != nil {
		return nil, err
	}

	start := time.Now()
	results := make([]*Result, len(cases))

	limit := r.Concurrency
	if limit < 1 {
		limit = 1
	}
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup

	for i, c := range cases {
		wg.Add(1)
		go func(i int, c Case) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = r.runCase(ctx, c)
		}(i, c)
	}
	wg.Wait()

	return &Report{
		Results:   results,
		Threshold: r.Threshold,
		Duration:  time.Since(start),
	}, nil
}

// checkNames rejects duplicates up front. Two cases sharing a name make a
// report ambiguous about which one regressed, and the mistake is easy to make
// when a suite is assembled from several files.
func (r *Runner) checkNames(cases []Case) error {
	seen := make(map[string]struct{}, len(cases))
	for _, c := range cases {
		if c.Name == "" {
			return fmt.Errorf("eval: every case needs a Name")
		}
		if len(c.Turns) == 0 {
			return fmt.Errorf("eval: case %q has no turns", c.Name)
		}
		if _, dup := seen[c.Name]; dup {
			return fmt.Errorf("eval: duplicate case name %q", c.Name)
		}
		seen[c.Name] = struct{}{}
	}
	return nil
}

func (r *Runner) runCase(ctx context.Context, c Case) *Result {
	start := time.Now()
	res := &Result{Case: c, Scores: map[string]Score{}}

	agent, err := r.Agent(ctx, c)
	if err != nil {
		res.Err = fmt.Errorf("building agent: %w", err)
		res.Duration = time.Since(start)
		return res
	}

	namespace := r.Namespace
	if namespace == "" {
		namespace = "eval"
	}
	// A fresh thread per case, so no case can see another's history. The uuid
	// keeps two runs of the same suite from sharing a thread in a persistence
	// adapter that outlives the process.
	threadID := "eval-" + c.Name + "-" + uuid.NewString()

	for i, turn := range c.Turns {
		out, err := agent.ExecuteWithoutTrace(ctx, &agents.AgentInput{
			Namespace: namespace,
			ThreadID:  threadID,
			Message:   messages.New("user", []responses.InputMessageUnion{userMessage(turn)}),
		})
		if err != nil {
			res.Err = fmt.Errorf("turn %d: %w", i+1, err)
			res.Duration = time.Since(start)
			return res
		}

		res.Outputs = append(res.Outputs, out)
		text, tools := extract(out)
		res.Tools = append(res.Tools, tools...)
		// Only the last turn's text is the answer under test; earlier turns
		// are setup. The tool trajectory spans the whole case, because that
		// is the behaviour a trajectory scorer is asking about.
		res.Text = text
	}

	res.Duration = time.Since(start)
	r.score(ctx, c, res)
	return res
}

func (r *Runner) score(ctx context.Context, c Case, res *Result) {
	for _, s := range r.Scorers {
		score, err := s.Score(ctx, c, res)
		if err != nil {
			// A scorer that could not reach a verdict fails the case rather
			// than silently counting as zero, which would look like the agent
			// misbehaved when in fact the measurement broke.
			res.Err = fmt.Errorf("scorer %s: %w", s.Name(), err)
			return
		}
		res.Scores[s.Name()] = score
	}
}
