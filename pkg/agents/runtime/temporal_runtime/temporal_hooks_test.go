package temporal_runtime_test

import (
	"context"
	"testing"
	"time"

	"github.com/hastekit/hastekit-sdk-go/pkg/agents/history"
	"go.temporal.io/sdk/activity"

	"github.com/hastekit/hastekit-sdk-go/pkg/agents"
	"github.com/hastekit/hastekit-sdk-go/pkg/agents/runtime/temporal_runtime"
	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/llm/responses"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"
)

// authzHook stands in for a hook that would call out to another service: the
// kind that has to be journaled rather than re-run on replay.
type authzHook struct {
	agents.NoopModelCallHook

	name    string
	deny    bool
	befores *int
	afters  *int
}

func (h *authzHook) GetName() string { return h.name }

func (h *authzHook) BeforeToolCall(ctx context.Context, call *agents.ToolCall) (agents.ToolCallHookResult, error) {
	*h.befores++
	if h.deny {
		return agents.HandleToolCall(agents.ToolCallResult(call, "denied by "+h.name)), nil
	}
	return agents.ContinueToolCall(), nil
}

func (h *authzHook) AfterToolCall(ctx context.Context, call *agents.ToolCall, result *agents.ToolCallResponse) (agents.ToolCallHookResult, error) {
	*h.afters++
	return agents.ContinueToolCall(), nil
}

func hookCall() *agents.ToolCall {
	return &agents.ToolCall{FunctionCallMessage: &responses.FunctionCallMessage{
		ID: "fc_1", CallID: "call_1", Name: "search",
	}}
}

// Each hook method is registered as its own activity, which is what lets the
// workflow run it as a separate durable step.
func TestToolCallHookActivities_RegisteredPerMethod(t *testing.T) {
	befores, afters := 0, 0

	agent := temporal_runtime.NewTemporalAgent(nil, &agents.AgentOptions{
		Name:    "Agent",
		History: newTestHistory(),
		Hooks:   []agents.Hook{&authzHook{name: "authz", befores: &befores, afters: &afters}},
	}, nil)

	// Scoped to the agent, like every other activity: two agents that both
	// name a hook "authz" must not register the same activity name, or one
	// silently replaces the other and an agent's calls are checked by another
	// agent's hook.
	activities := agent.GetActivities()
	assert.Contains(t, activities, "Agent_authz_BeforeToolCallActivity")
	assert.Contains(t, activities, "Agent_authz_AfterToolCallActivity")

	other := temporal_runtime.NewTemporalAgent(nil, &agents.AgentOptions{
		Name:    "Other",
		History: newTestHistory(),
		Hooks:   []agents.Hook{&authzHook{name: "authz", befores: &befores, afters: &afters}},
	}, nil)
	assert.Contains(t, other.GetActivities(), "Other_authz_BeforeToolCallActivity")
}

// A toolset with no hooks registers none — the common case adds no activities.
func TestToolCallHookActivities_NoneWithoutHooks(t *testing.T) {
	agent := temporal_runtime.NewTemporalAgent(nil, &agents.AgentOptions{
		Name:    "Agent",
		History: newTestHistory(),
	}, nil)

	for name := range agent.GetActivities() {
		assert.NotContains(t, name, "ToolCallActivity")
	}
}

// The activities are the hook's own methods, so what the workflow journals is
// the hook's real decision.
func TestToolCallHookActivity_RunsTheHook(t *testing.T) {
	befores, afters := 0, 0
	hook := &authzHook{name: "authz", deny: true, befores: &befores, afters: &afters}

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(hook.BeforeToolCall)

	val, err := env.ExecuteActivity(hook.BeforeToolCall, hookCall())
	require.NoError(t, err)

	var out agents.ToolCallHookResult
	require.NoError(t, val.Get(&out))
	require.True(t, out.Handled, "the hook says it answered the call itself")
	require.NotNil(t, out.Response)
	assert.Equal(t, "denied by authz", *out.Response.Output.OfString)
	assert.Equal(t, 1, befores)
}

// The tool executor runs every hook method as its own activity, in order,
// around the tool's own activity.
func TestTemporalToolExecutor_RunsHooksAsSeparateActivities(t *testing.T) {
	var calls []string

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()

	// Each hook method and the tool call are registered separately; recording
	// the order they run in is what proves they are distinct steps.
	env.RegisterActivityWithOptions(
		func(ctx context.Context, call *agents.ToolCall) (agents.ToolCallHookResult, error) {
			calls = append(calls, "before:authz")
			return agents.ContinueToolCall(), nil
		}, activityNamed("authz_BeforeToolCallActivity"))
	env.RegisterActivityWithOptions(
		func(ctx context.Context, call *agents.ToolCall, result *agents.ToolCallResponse) (agents.ToolCallHookResult, error) {
			calls = append(calls, "after:authz")
			return agents.ContinueToolCall(), nil
		}, activityNamed("authz_AfterToolCallActivity"))
	env.RegisterActivityWithOptions(
		func(ctx context.Context, call *agents.ToolCall) (*agents.ToolCallResponse, error) {
			calls = append(calls, "exec")
			return agents.ToolCallResult(call, "tool ran"), nil
		}, activityNamed("Agent_worker_ExecuteToolActivity"))

	env.RegisterWorkflow(hookProxyWorkflow)
	env.ExecuteWorkflow(hookProxyWorkflow)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var out string
	require.NoError(t, env.GetWorkflowResult(&out))
	assert.Equal(t, "tool ran", out)
	assert.Equal(t, []string{"before:authz", "exec", "after:authz"}, calls)
}

// hookProxyWorkflow drives the executor the way the agent loop would: the
// executor is where hooks run, so this is the path a real run takes.
func hookProxyWorkflow(ctx workflow.Context) (string, error) {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: testActivityTimeout,
	})

	executor := temporal_runtime.NewTemporalToolExecutor(ctx).WithToolCallHooks(
		[]agents.ToolCallHook{temporal_runtime.NewTemporalHookProxy(ctx, "authz")},
	)

	results := executor.ExecuteAll(context.Background(), []agents.ExecutableToolCall{{
		ToolName: "worker",
		Tool:     temporal_runtime.NewTemporalToolProxy(ctx, "Agent_worker", nil),
		ToolCall: hookCall(),
	}})
	if results[0].Err != nil {
		return "", results[0].Err
	}
	return *results[0].Response.Output.OfString, nil
}

const testActivityTimeout = 10 * time.Second

func activityNamed(name string) activity.RegisterOptions {
	return activity.RegisterOptions{Name: name}
}

func newTestHistory() *history.CommonConversationManager {
	return history.NewConversationManager(history.NewInMemoryConversationPersistence())
}

// creditsHook stands in for a balance check that talks to a billing service —
// the kind that must be journaled rather than charged again on replay.
type creditsHook struct {
	agents.NoopToolCallHook
	name string
}

func (h *creditsHook) GetName() string { return h.name }

func (h *creditsHook) BeforeModelCall(ctx context.Context, call *agents.ModelCall) (agents.ModelCallHookResult, error) {
	return agents.ContinueModelCall(), nil
}

func (h *creditsHook) AfterModelCall(ctx context.Context, call *agents.ModelCall, result *agents.ModelCallResult) (agents.ModelCallHookResult, error) {
	return agents.ContinueModelCall(), nil
}

// Model call hooks get their own activities, scoped to the agent like every
// other one.
func TestModelCallHookActivities_RegisteredPerMethod(t *testing.T) {
	agent := temporal_runtime.NewTemporalAgent(nil, &agents.AgentOptions{
		Name:    "Agent",
		History: newTestHistory(),
		Hooks:   []agents.Hook{&creditsHook{name: "credits"}},
	}, nil)

	activities := agent.GetActivities()
	assert.Contains(t, activities, "Agent_credits_BeforeModelCallActivity")
	assert.Contains(t, activities, "Agent_credits_AfterModelCallActivity")
}

// The proxy runs each phase as its own activity around the model's own call,
// so a balance check is asked once and replayed thereafter.
func TestTemporalModelCallHookProxy_RunsEachPhaseAsAnActivity(t *testing.T) {
	var calls []string

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()

	env.RegisterActivityWithOptions(
		func(ctx context.Context, call *agents.ModelCall) (agents.ModelCallHookResult, error) {
			calls = append(calls, "before:"+call.Model)
			return agents.ContinueModelCall(), nil
		}, activityNamed("credits_BeforeModelCallActivity"))
	env.RegisterActivityWithOptions(
		func(ctx context.Context, call *agents.ModelCall, result *agents.ModelCallResult) (agents.ModelCallHookResult, error) {
			calls = append(calls, "after:charged")
			chargedTotal = result.Usage.TotalTokens
			return agents.ContinueModelCall(), nil
		}, activityNamed("credits_AfterModelCallActivity"))

	env.RegisterWorkflow(modelHookWorkflow)
	env.ExecuteWorkflow(modelHookWorkflow)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var out string
	require.NoError(t, env.GetWorkflowResult(&out))
	// Both phases ran as their own activities. The model call itself is inline
	// in this test rather than an activity, so it isn't in this log; the reply
	// below is what shows it happened between them.
	assert.Equal(t, "the model answered", out)
	assert.Equal(t, []string{"before:gpt-x", "after:charged"}, calls)

	// The after hook is told what the call reported, which is what a balance is
	// drawn down by — it has to survive the data converter to be worth having.
	assert.Equal(t, 154, chargedTotal)
}

var chargedTotal int

func modelHookWorkflow(ctx workflow.Context) (string, error) {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: testActivityTimeout,
	})

	hooks := []agents.ModelCallHook{temporal_runtime.NewTemporalHookProxy(ctx, "credits")}

	resp, err := agents.RunWithModelCallHooks(context.Background(), hooks,
		&agents.ModelCall{AgentName: "Agent", Model: "gpt-x"},
		func(context.Context) (*responses.Response, error) {
			reply := agents.ModelCallText("the model answered")
			reply.Usage = &responses.Usage{InputTokens: 120, OutputTokens: 34, TotalTokens: 154}
			return reply, nil
		})
	if err != nil {
		return "", err
	}
	return (*resp.Output[0].OfOutputMessage.Content)[0].OfOutputText.Text, nil
}
