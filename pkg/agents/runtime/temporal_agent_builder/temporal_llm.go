package temporal_agent_builder

import (
	"context"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/runtime/temporal_runtime"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"go.temporal.io/sdk/workflow"
)

const llmCallActivity = WorkflowName + "_LLMCallActivity"

// LLMCallInput is the model call activity's input: the request and its call,
// and the model id the worker resolves through its allowlist.
type LLMCallInput struct {
	Model   string             `json:"model"`
	Request *responses.Request `json:"request"`
	Call    *agents.ModelCall  `json:"call"`
}

// LLMCall resolves the config's model and calls it the way a static agent's
// model activity does: stop-aware, streaming to the broker, with the Builder's
// model-call middlewares wrapping it.
func (a *AgentBuilder) LLMCall(ctx context.Context, in *LLMCallInput) (*responses.Response, error) {
	provider, err := a.builder.Model(in.Model)
	if err != nil {
		return nil, configError(err)
	}
	llm := temporal_runtime.NewTemporalLLM(provider, a.broker, agents.ModelCallMiddlewaresOf(a.builder.Middlewares())...)
	return llm.NewStreamingResponsesActivity(ctx, in.Request, in.Call)
}

// llmProxy is the workflow-side model: every call is an LLMCall activity
// carrying the config's model id.
type llmProxy struct {
	ctx   workflow.Context
	model string
}

func newLLMProxy(ctx workflow.Context, model string) agents.LLM {
	return &llmProxy{ctx: ctx, model: model}
}

func (l *llmProxy) NewStreamingResponses(_ context.Context, call *agents.ModelCall, in *responses.Request, _ func(chunk *responses.ResponseChunk)) (*responses.Response, error) {
	var response *responses.Response
	err := workflow.ExecuteActivity(l.ctx, llmCallActivity, &LLMCallInput{Model: l.model, Request: in, Call: call}).Get(l.ctx, &response)
	if err != nil {
		// A call the stop cut short is reported as the stop it is, so the loop
		// ends the run cleanly instead of failing it.
		if temporal_runtime.WasStopped(err) {
			return nil, agents.ErrModelCallStopped
		}
		return nil, err
	}
	return response, nil
}
