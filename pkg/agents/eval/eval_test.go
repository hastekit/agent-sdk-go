package eval_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/eval"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
)

// scriptedLLM replays canned responses in order, so a case exercises the real
// agent loop without a provider.
type scriptedLLM struct {
	mu     sync.Mutex
	script []*responses.Response
	calls  int
}

func (s *scriptedLLM) NewStreamingResponses(ctx context.Context, _ *agents.ModelCall, in *responses.Request, cb func(*responses.ResponseChunk)) (*responses.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.calls >= len(s.script) {
		return textResponse(""), nil
	}
	out := s.script[s.calls]
	s.calls++
	return out, nil
}

func textResponse(text string) *responses.Response {
	return &responses.Response{
		Output: []responses.OutputMessageUnion{{
			OfOutputMessage: &responses.OutputMessage{
				ID:      "msg-1",
				Role:    constants.RoleAssistant,
				Content: &responses.OutputContent{{OfOutputText: &responses.OutputTextContent{Text: text}}},
			},
		}},
		Usage: &responses.Usage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
	}
}

func toolCallResponse(name string) *responses.Response {
	return &responses.Response{
		Output: []responses.OutputMessageUnion{{
			OfFunctionCall: &responses.FunctionCallMessage{ID: "fc-1", CallID: "call-1", Name: name, Arguments: "{}"},
		}},
		Usage: &responses.Usage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
	}
}

// agentSaying builds an agent whose model replays the given script.
func agentSaying(t *testing.T, script ...*responses.Response) *agents.Agent {
	t.Helper()
	hist := history.NewConversationManager(history.NewInMemoryConversationPersistence())
	return agents.NewAgent(&agents.AgentOptions{
		Name:    "under-test",
		History: hist,
	}).WithLLM(&scriptedLLM{script: script})
}

// TestRunnerScoresAPassingCase drives the whole harness end to end: a real
// agent loop, a real run, and a scorer reading what came back.
func TestRunnerScoresAPassingCase(t *testing.T) {
	r := &eval.Runner{
		Agent: func(context.Context, eval.Case) (*agents.Agent, error) {
			return agentSaying(t, textResponse("The capital of France is Paris.")), nil
		},
		Scorers:   []eval.Scorer{eval.Contains{}},
		Threshold: 1,
	}

	rep, err := r.Run(context.Background(), []eval.Case{{
		Name:   "capital-of-france",
		Turns:  []string{"What is the capital of France?"},
		Expect: eval.Expect{Text: "Paris"},
	}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if rep.Failed() != 0 {
		t.Fatalf("expected a pass, got:\n%s", rep)
	}
	if got := rep.Results[0].Scores["contains"].Value; got != 1 {
		t.Errorf("contains score = %v, want 1", got)
	}
	if got := rep.Results[0].Text; got != "The capital of France is Paris." {
		t.Errorf("captured text = %q", got)
	}
}

// TestRunnerReportsAFailure checks the failure path carries enough to diagnose
// the regression: which case, which scorer, and what it saw.
func TestRunnerReportsAFailure(t *testing.T) {
	r := &eval.Runner{
		Agent: func(context.Context, eval.Case) (*agents.Agent, error) {
			return agentSaying(t, textResponse("The capital of France is Lyon.")), nil
		},
		Scorers:   []eval.Scorer{eval.Contains{}},
		Threshold: 1,
	}

	rep, err := r.Run(context.Background(), []eval.Case{{
		Name:   "capital-of-france",
		Turns:  []string{"What is the capital of France?"},
		Expect: eval.Expect{Text: "Paris"},
	}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if rep.Failed() != 1 {
		t.Fatalf("expected 1 failure, got %d", rep.Failed())
	}
	out := rep.String()
	for _, want := range []string{"FAIL capital-of-france", "contains", "Paris", "Lyon"} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q:\n%s", want, out)
		}
	}
}

// TestToolTrajectoryCapturedAcrossTurns covers the metric a text scorer cannot
// see: whether the agent actually used its tools to get there.
func TestToolTrajectoryCapturedAcrossTurns(t *testing.T) {
	lookup := &recordingTool{name: "lookup"}

	r := &eval.Runner{
		Agent: func(context.Context, eval.Case) (*agents.Agent, error) {
			hist := history.NewConversationManager(history.NewInMemoryConversationPersistence())
			return agents.NewAgent(&agents.AgentOptions{
				Name:    "under-test",
				History: hist,
				Tools:   []agents.Tool{lookup},
			}).WithLLM(&scriptedLLM{script: []*responses.Response{
				toolCallResponse("lookup"),
				textResponse("It is Paris."),
			}}), nil
		},
		Scorers:   []eval.Scorer{eval.ToolTrajectory{Ordered: true}, eval.Contains{}},
		Threshold: 1,
	}

	rep, err := r.Run(context.Background(), []eval.Case{{
		Name:  "looks-it-up",
		Turns: []string{"What is the capital of France?"},
		Expect: eval.Expect{
			Text:  "Paris",
			Tools: []string{"lookup"},
		},
	}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if rep.Failed() != 0 {
		t.Fatalf("expected a pass, got:\n%s", rep)
	}
	if got := rep.Results[0].Tools; len(got) != 1 || got[0] != "lookup" {
		t.Errorf("captured tools = %v, want [lookup]", got)
	}
}

// TestToolTrajectoryCatchesTheGuess is the failure this scorer exists for: the
// answer is right, so the text scorer is happy, but the agent never looked
// anything up.
func TestToolTrajectoryCatchesTheGuess(t *testing.T) {
	r := &eval.Runner{
		Agent: func(context.Context, eval.Case) (*agents.Agent, error) {
			return agentSaying(t, textResponse("It is Paris.")), nil
		},
		Scorers:   []eval.Scorer{eval.ToolTrajectory{}, eval.Contains{}},
		Threshold: 1,
	}

	rep, err := r.Run(context.Background(), []eval.Case{{
		Name:   "should-have-looked-it-up",
		Turns:  []string{"What is the capital of France?"},
		Expect: eval.Expect{Text: "Paris", Tools: []string{"lookup"}},
	}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	res := rep.Results[0]
	if res.Scores["contains"].Value != 1 {
		t.Errorf("text scorer should pass; the answer is correct")
	}
	if res.Scores["tool_trajectory"].Value != 0 {
		t.Errorf("trajectory scorer should fail; no tool was called")
	}
	if rep.Failed() != 1 {
		t.Errorf("case should fail on the mean, got:\n%s", rep)
	}
}

// TestPerCaseThreshold covers a scenario allowed to be held to a lower bar.
func TestPerCaseThreshold(t *testing.T) {
	lenient := 0.4
	r := &eval.Runner{
		Agent: func(context.Context, eval.Case) (*agents.Agent, error) {
			return agentSaying(t, textResponse("It is Paris.")), nil
		},
		Scorers:   []eval.Scorer{eval.ToolTrajectory{}, eval.Contains{}},
		Threshold: 1,
	}

	rep, err := r.Run(context.Background(), []eval.Case{{
		Name:      "lenient",
		Turns:     []string{"What is the capital of France?"},
		Expect:    eval.Expect{Text: "Paris", Tools: []string{"lookup"}},
		Threshold: &lenient,
	}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Failed() != 0 {
		t.Fatalf("per-case threshold not honored:\n%s", rep)
	}
}

// TestMultiTurnSharesAThread checks that later turns see the earlier ones, and
// that the scored text is the last turn's rather than a concatenation.
func TestMultiTurnSharesAThread(t *testing.T) {
	r := &eval.Runner{
		Agent: func(context.Context, eval.Case) (*agents.Agent, error) {
			return agentSaying(t,
				textResponse("Hello there."),
				textResponse("Your name is Ada."),
			), nil
		},
		Scorers:   []eval.Scorer{eval.Contains{}},
		Threshold: 1,
	}

	rep, err := r.Run(context.Background(), []eval.Case{{
		Name:   "remembers-the-name",
		Turns:  []string{"Hi, my name is Ada.", "What is my name?"},
		Expect: eval.Expect{Text: "Ada"},
	}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	res := rep.Results[0]
	if len(res.Outputs) != 2 {
		t.Fatalf("expected 2 turn outputs, got %d", len(res.Outputs))
	}
	if res.Text != "Your name is Ada." {
		t.Errorf("scored text = %q, want only the final turn", res.Text)
	}
}

// TestRunnerRejectsBadSuites covers the up-front validation. A duplicate name
// makes a report ambiguous about which scenario regressed.
func TestRunnerRejectsBadSuites(t *testing.T) {
	r := &eval.Runner{
		Agent:   func(context.Context, eval.Case) (*agents.Agent, error) { return agentSaying(t), nil },
		Scorers: []eval.Scorer{eval.Contains{}},
	}

	tests := []struct {
		name  string
		cases []eval.Case
	}{
		{"no cases", nil},
		{"unnamed case", []eval.Case{{Turns: []string{"hi"}}}},
		{"no turns", []eval.Case{{Name: "empty"}}},
		{"duplicate names", []eval.Case{
			{Name: "dup", Turns: []string{"a"}},
			{Name: "dup", Turns: []string{"b"}},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := r.Run(context.Background(), tt.cases); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

// TestScorerErrorFailsTheCase pins the distinction between a bad verdict and no
// verdict: a broken measurement must not read as a misbehaving agent.
func TestScorerErrorFailsTheCase(t *testing.T) {
	r := &eval.Runner{
		Agent: func(context.Context, eval.Case) (*agents.Agent, error) {
			return agentSaying(t, textResponse("fine")), nil
		},
		Scorers: []eval.Scorer{eval.ScorerFunc{
			Label: "broken",
			Fn: func(context.Context, eval.Case, *eval.Result) (eval.Score, error) {
				return eval.Score{}, context.DeadlineExceeded
			},
		}},
	}

	rep, err := r.Run(context.Background(), []eval.Case{{Name: "c", Turns: []string{"hi"}}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Results[0].Err == nil {
		t.Fatal("a scorer error should fail the case")
	}
	if rep.Failed() != 1 {
		t.Fatal("errored case must not pass")
	}
}

// recordingTool is a no-op tool that exists so the agent has something to call.
type recordingTool struct{ name string }

func (t *recordingTool) GetToolDescriptor() *agents.BaseTool {
	return &agents.BaseTool{
		Name: t.name,
		ToolUnion: responses.ToolUnion{OfFunction: &responses.FunctionTool{
			Type: "function",
			Name: t.name,
		}},
	}
}

func (t *recordingTool) Execute(ctx context.Context, params *agents.ToolCall) (*agents.ToolCallResponse, error) {
	return &agents.ToolCallResponse{
		FunctionCallOutputMessage: &responses.FunctionCallOutputMessage{
			CallID: params.CallID,
			Output: responses.FunctionCallOutputContentUnion{OfString: strPtr("Paris")},
		},
	}, nil
}

func strPtr(s string) *string { return &s }
