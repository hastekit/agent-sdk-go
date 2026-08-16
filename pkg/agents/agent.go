package agents

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"

	"github.com/bytedance/sonic"
	"github.com/google/uuid"
	"github.com/hastekit/hastekit-sdk-go/pkg/agents/agentstate"
	"github.com/hastekit/hastekit-sdk-go/pkg/agents/history"
	"github.com/hastekit/hastekit-sdk-go/pkg/agents/messages"
	"github.com/hastekit/hastekit-sdk-go/pkg/agents/streambroker"
	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/llm"
	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/hastekit-sdk-go/pkg/genai"
	"github.com/hastekit/hastekit-sdk-go/pkg/utils"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

var (
	tracer = otel.Tracer("Agent")
)

type Agent struct {
	Name           string
	output         map[string]any
	history        *history.CommonConversationManager
	instruction    SystemPromptProvider
	tools          []Tool
	mcpServers     []MCPToolset
	llm            LLM
	parameters     responses.Parameters
	runtime        Runtime
	maxLoops       int
	streamBroker   StreamBroker
	handoffs       []*Handoff
	toolExecutor   ToolExecutor
	durableStep    DurableStep
	stickyHandoff  bool
	singleTurn     bool
	modelCallHooks []ModelCallHook
	skills         SkillProvider
}

type AgentOptions struct {
	History     *history.CommonConversationManager
	Instruction SystemPromptProvider
	Parameters  responses.Parameters

	Name     string
	LLM      llm.Provider
	Output   map[string]any
	Tools    []Tool
	Handoffs []*Handoff

	// Skills are folders of instructions the agent reads only when it needs
	// them — see SkillProvider and NewSkillRegistryFromDir. The agent lists
	// them in its prompt and adds the provider's reader tool to Tools itself,
	// so the two halves cannot be wired up inconsistently.
	Skills        SkillProvider
	McpServers    []MCPToolset
	Runtime       Runtime
	MaxLoops      *int
	ToolExecutor  ToolExecutor
	StreamBroker  StreamBroker
	DurableStep   DurableStep
	StickyHandoff bool

	// Hooks observe or intercept what the agent does — see Hook. Each wraps
	// both sides, with NoopToolCallHook or NoopModelCallHook standing in for
	// the side it does not care about:
	//
	//   - the tool-call side wraps every tool the agent calls: its own function
	//     tools, its sub-agent tools, and every MCP server's. Handoffs do not
	//     pass through it, since transfer_to_agent calls out to nothing and the
	//     target agent's own hooks govern what it then does.
	//   - the model-call side wraps every call to the model, one per turn of
	//     the tool loop. A budget check belongs here.
	//
	// Tool hooks are handed to the ToolExecutor, which runs them (see
	// HookAwareToolExecutor); model hooks run in the loop, where the model is
	// called. Under Temporal and Restate both of those sit inside the workflow,
	// so a hook's methods become journaled steps either way.
	Hooks []Hook

	// SingleTurn ends the run as soon as the model has responded, before any
	// tool is executed. The returned AgentOutput carries exactly what the model
	// emitted — assistant text and/or tool calls — with status completed.
	//
	// This is for evaluating a single decision rather than an outcome: offline
	// single-turn evals score "given this conversation, what does the model do
	// next?", where executing the tool (or faking its result) would replace the
	// thing under test with an improvised continuation.
	//
	// Note this is not the same as MaxLoops=1, which still executes the first
	// round of tools and then fails the run with "exceeded maximum loops".
	SingleTurn bool
}

func NewAgent(opts *AgentOptions) *Agent {
	maxLoops := 50
	if opts.MaxLoops != nil && *opts.MaxLoops > 0 {
		maxLoops = *opts.MaxLoops
	}

	if opts.Output != nil {
		format := map[string]any{
			"type":   "json_schema",
			"name":   "structured_output",
			"strict": false,
			"schema": opts.Output,
		}
		opts.Parameters.Text = &responses.TextFormat{
			Format: format,
		}
	}

	if opts.History == nil {
		opts.History = history.NewConversationManager(history.NewInMemoryConversationPersistence())
	}

	durableStep := opts.DurableStep
	if durableStep == nil {
		durableStep = localDurableStep{}
	}

	toolExecutor := opts.ToolExecutor
	if toolExecutor == nil {
		toolExecutor = &DefaultToolExecutor{}
	}

	streamBroker := opts.StreamBroker
	if streamBroker == nil {
		streamBroker = streambroker.NewMemoryStreamBroker()
	}

	// Hand the executor the same broker the loop uses, so it can watch the
	// stop flag. Executors with no such need don't implement this.
	if aware, ok := toolExecutor.(BrokerAwareToolExecutor); ok {
		toolExecutor = aware.WithStreamBroker(streamBroker)
	}

	// Hand the executor the tool-call side, since running those around each
	// call is its job. Only when the agent was given some: an executor built
	// with hooks of its own keeps them rather than having them cleared.
	if aware, ok := toolExecutor.(HookAwareToolExecutor); ok && len(opts.Hooks) > 0 {
		toolExecutor = aware.WithToolCallHooks(ToolCallHooksOf(opts.Hooks))
	}

	return &Agent{
		Name:        opts.Name,
		output:      opts.Output,
		history:     opts.History,
		instruction: opts.Instruction,
		// The skill source brings its own reader tool, so an agent given
		// skills can always read them — there is no second thing to remember
		// to pass, and no way to advertise a skill the model cannot open.
		tools:          WithSkillTool(opts.Tools, opts.Skills),
		skills:         opts.Skills,
		mcpServers:     opts.McpServers,
		llm:            &WrappedLLM{opts.LLM},
		parameters:     opts.Parameters,
		runtime:        opts.Runtime,
		maxLoops:       maxLoops,
		handoffs:       opts.Handoffs,
		toolExecutor:   toolExecutor,
		streamBroker:   streamBroker,
		durableStep:    durableStep,
		stickyHandoff:  opts.StickyHandoff,
		singleTurn:     opts.SingleTurn,
		modelCallHooks: ModelCallHooksOf(opts.Hooks),
	}
}

// WithLLM returns a copy of the agent bound to a different LLM. It copies the
// struct wholesale rather than listing fields: this used to enumerate them,
// and a field added anywhere else was silently dropped here — the copy simply
// lost it, with nothing to fail until the behaviour went missing at runtime.
func (e *Agent) WithLLM(wrappedLLM LLM) *Agent {
	clone := *e
	clone.llm = wrappedLLM
	return &clone
}

func (e *Agent) PrepareMCPTools(ctx context.Context, runContext map[string]any) ([]Tool, error) {
	coreTools := []Tool{}
	if e.mcpServers != nil {
		for _, mcpServer := range e.mcpServers {
			mcpTools, err := mcpServer.ListTools(ctx, runContext)
			if err != nil {
				return nil, fmt.Errorf("failed to list MCP tools: %w", err)
			}

			coreTools = append(coreTools, mcpTools...)
		}
	}

	return coreTools, nil
}

// AddHandoffs appends handoff edges to the agent after construction.
func (e *Agent) AddHandoffs(handoffs ...*Handoff) {
	e.handoffs = append(e.handoffs, handoffs...)
}

func (e *Agent) PrepareHandoffTools(ctx context.Context) []Tool {
	coreTools := []Tool{}

	if e.handoffs != nil && len(e.handoffs) > 0 {
		coreTools = append(coreTools, NewHandoffTool(&responses.ToolUnion{
			OfFunction: &responses.FunctionTool{
				Name:        "transfer_to_agent",
				Description: utils.Ptr("Transfer the conversation to another agent"),
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"agent_name": map[string]any{
							"type":        "string",
							"description": "Name of the target agent",
						},
					},
					"required": []string{"agent_name"},
				},
			},
		}))
	}

	return coreTools
}

func (e *Agent) GetRunID(ctx context.Context) string {
	return uuid.NewString()
}

// History returns the agent's conversation manager. Callers can use it
// to inspect stored conversations, e.g. listing threads when the
// persistence adapter implements history.ThreadLister.
func (e *Agent) History() *history.CommonConversationManager {
	return e.history
}

// Stop asks the run on streamID to stop — the same request
// AgentHandle.Stop makes, for callers that don't hold the handle.
//
// It goes to the broker, so with a shared broker any process holding this
// agent can stop a run streaming in another. Returning means the request
// was recorded, not that a run was there to receive it: stopping a
// finished run, or an unknown stream id, is not an error.
func (e *Agent) Stop(ctx context.Context, streamID string) error {
	return e.streamBroker.Stop(ctx, streamID)
}

// StreamBroker returns the broker the agent streams through, for callers
// that need the run's channel directly — rejoining a stream in flight, or
// folding a turn into a live run (see RunClaimBroker).
func (e *Agent) StreamBroker() StreamBroker {
	return e.streamBroker
}

// ToolExecutor returns the executor the agent runs tool calls through — the
// one it was configured with, after the broker and the agent's tool call hooks
// were injected into it.
func (e *Agent) ToolExecutor() ToolExecutor {
	return e.toolExecutor
}

type AgentInput struct {
	Namespace     string          `json:"namespace"`
	ThreadID      string          `json:"thread_id"`
	PreviousRunID string          `json:"previous_run_id"`
	Message       history.Message `json:"messages"`
	RunContext    map[string]any  `json:"run_context"`

	// StreamID is the broker channel used for streaming chunks and for
	// stop signaling. The runtime and the agent loop use it to publish
	// and to poll IsStopped. Execute generates one if empty.
	StreamID string `json:"stream_id,omitempty"`

	// This is the conversation ID shared by the parent agent and the sub-agent.
	SessionID string `json:"shared_session_id"`
}

// AgentOutput represents the result of agent execution
type AgentOutput struct {
	RunID      string                        `json:"run_id"`
	Status     agentstate.RunStatus          `json:"status"`
	Output     []responses.InputMessageUnion `json:"output"`
	Interrupts []responses.Interrupt         `json:"interrupts,omitempty"`
}

// Execute is the single public entry point for running the agent. It
// generates a StreamID (if not supplied), subscribes to the configured
// stream broker for that channel, and launches the run in a background
// goroutine. The returned handle exposes a chunk channel, a Stop
// function for clean cancellation, and a Wait function for the final
// AgentOutput. The agent itself publishes all chunks (run lifecycle,
// LLM streaming, tool results) through the broker — there is no
// callback API.
func (e *Agent) Execute(ctx context.Context, in *AgentInput) (*AgentHandle, error) {
	if e.streamBroker == nil {
		return nil, fmt.Errorf("Execute requires a stream broker on the agent")
	}

	if in.StreamID == "" {
		in.StreamID = uuid.NewString()
	}

	chunks, err := e.streamBroker.Subscribe(ctx, in.StreamID)
	if err != nil {
		return nil, fmt.Errorf("failed to subscribe to stream: %w", err)
	}

	handle := &AgentHandle{
		StreamID: in.StreamID,
		Chunks:   chunks,
		broker:   e.streamBroker,
		done:     make(chan struct{}),
	}

	if in.ThreadID == "" {
		in.ThreadID = uuid.NewString()
	}

	go func() {
		defer close(handle.done)

		// GenAI invoke_agent span for the whole run. Callers that don't want
		// this span invoke ExecuteWithoutTrace directly instead.
		runCtx, span := tracer.Start(context.WithoutCancel(ctx), genai.OpInvokeAgent+" "+e.Name)
		defer span.End()
		span.SetAttributes(
			attribute.String(genai.AttrOperationName, genai.OpInvokeAgent),
			attribute.String(genai.AttrAgentName, e.Name),
			attribute.String(genai.AttrConversationID, in.ThreadID),
			attribute.String(genai.AttrSessionID, in.ThreadID),
		)

		if s, ok := genai.InputMessages(in.Message.Messages); ok {
			span.SetAttributes(attribute.String(genai.AttrInputMessages, s))
		}

		// Stream lifecycle (Subscribe/Close) is owned by ExecuteLocal,
		// which is the loop runner. We just observe it here.
		handle.result, handle.err = e.ExecuteWithoutTrace(runCtx, in)

		if handle.result != nil {
			if s, ok := genai.OutputMessages(handle.result.Output); ok {
				span.SetAttributes(attribute.String(genai.AttrOutputMessages, s))
			}
		}
	}()

	return handle, nil
}

// ExecuteWithoutTrace dispatches to the configured runtime, falling back
// to the in-process loop when none is set. Runtime workflow handlers
// (Temporal, Restate) call this directly after constructing their proxy
// agent so it skips tracing setup.
func (e *Agent) ExecuteWithoutTrace(ctx context.Context, in *AgentInput) (*AgentOutput, error) {
	if e.runtime != nil {
		return e.runtime.Run(ctx, e, in)
	}
	return e.ExecuteLocal(ctx, in)
}

// ExecuteLocal runs the agent's state machine in the calling goroutine.
// LocalRuntime calls this inside its goroutine; ExecuteWithoutTrace calls
// it when no runtime is set.
//
// ExecuteLocal owns the broker stream's lifecycle: it closes the stream
// channel on return so subscribers terminate cleanly. Callers (Agent.Execute,
// the gateway's runtime workflows, etc.) don't need to call Close themselves.
func (e *Agent) ExecuteLocal(ctx context.Context, in *AgentInput) (*AgentOutput, error) {
	if e.streamBroker != nil && in.StreamID != "" {
		defer e.streamBroker.Close(context.Background(), in.StreamID)
	}

	run, err := history.NewRun(ctx, e.history, in.Namespace, in.ThreadID, in.PreviousRunID, history.WithRunContext(in.RunContext))
	if err != nil {
		return &AgentOutput{Status: agentstate.RunStatusError, RunID: ""}, err
	}

	// Add the incoming message to the run
	run.AddMessages(ctx, in.Message)

	runId := run.GetRunID()

	if in.SessionID == "" {
		in.SessionID = run.GetConversationID()
	}

	// The run's trace id comes from the ambient span (an externally-provided
	// invoke_agent span, or the surrounding workflow span) and is echoed in the
	// run.* chunks for client correlation.
	var traceid string
	if sc := trace.SpanFromContext(ctx).SpanContext(); sc.IsValid() {
		traceid = sc.TraceID().String()
	}
	run.RunState.TraceID = traceid

	// Emit run.created once (durable step: not resent on replay).
	e.durableStep.Do(func() {
		e.runCreated(ctx, in.StreamID, runId, traceid)
	})

	// Sticky handoff: if a prior turn on this thread ended inside a
	// specialist reached via handoff, resume there instead of
	// re-entering this root agent. LastAgentName was carried forward by
	// NewRun and hasn't been overwritten yet (GetMessages does that, and
	// it runs later inside the loop). Routing happens only here at the
	// top-level entry — never in ExecuteWithRun, which is also the
	// handoff target's own entry point.
	if e.stickyHandoff {
		if target := e.stickyHandoffTarget(run.RunState.LastAgentName); target != nil {
			return target.ExecuteWithRun(ctx, in, run)
		}
	}

	return e.ExecuteWithRun(ctx, in, run)
}
func (e *Agent) ExecuteWithRun(ctx context.Context, in *AgentInput, run *history.ConversationRunManager) (*AgentOutput, error) {
	publish := e.publisher(in.StreamID)

	if in.SessionID == "" {
		in.SessionID = run.GetConversationID()
	}

	handoffTools := e.PrepareHandoffTools(ctx)
	tools := append(e.tools, handoffTools...)

	// Connect to MCP servers, and list the tools
	mcpTools, err := e.PrepareMCPTools(ctx, in.RunContext)
	if err != nil {
		return nil, err
	}

	// Merge MCP tools with other tools
	tools = append(tools, mcpTools...)

	// Create tool schemas for input payload
	var toolDefs []responses.ToolUnion
	var deferredTools []Tool
	if len(tools) > 0 {
		// Collect deferred tools
		for _, coreTool := range tools {
			if coreTool.IsDeferred() {
				deferredTools = append(deferredTools, coreTool)
			}
		}

		// Add a search tool if any deferred tools are present
		if len(deferredTools) > 0 {
			tools = append(tools, NewToolSearchTool(deferredTools))
		}

		// Convert core tools to tool definitions
		toolDefs = make([]responses.ToolUnion, len(tools)-len(deferredTools))
		idx := 0
		for _, coreTool := range tools {
			// Skip deferred tools
			if coreTool.IsDeferred() {
				continue
			}

			toolDefs[idx] = *coreTool.Tool(ctx)
			idx += 1
		}
	}

	// Load run state from meta (in-memory, no DB call)
	runId := run.GetRunID()

	// Get the prompt
	instruction := "You are a helpful assistant."
	if e.instruction != nil {
		// Project deferred tools into a JSON-serializable view so the
		// Dependencies struct survives Temporal's activity boundary.
		var deferredToolInfos []DeferredToolInfo
		if len(deferredTools) > 0 {
			deferredToolInfos = make([]DeferredToolInfo, 0, len(deferredTools))
			for _, dt := range deferredTools {
				schema := dt.Tool(ctx)
				if schema == nil || schema.OfFunction == nil {
					continue
				}
				info := DeferredToolInfo{Name: schema.OfFunction.Name}
				if schema.OfFunction.Description != nil {
					info.Description = strings.SplitN(strings.TrimPrefix(*schema.OfFunction.Description, "\n"), "\n", 2)[0]
				}
				deferredToolInfos = append(deferredToolInfos, info)
			}
		}

		skills, skillToolName := skillDependencies(ctx, e.skills)

		instruction, err = e.instruction.GetPrompt(ctx, &Dependencies{
			RunContext:    in.RunContext,
			Handoffs:      e.handoffs,
			DeferredTools: deferredToolInfos,
			Skills:        skills,
			SkillToolName: skillToolName,
		})
		if err != nil {
			return &AgentOutput{Status: agentstate.RunStatusError, RunID: runId}, err
		}
	}

	// Apply structured output format if configured
	parameters := e.parameters
	if e.output != nil {
		format := map[string]any{
			"type":   "json_schema",
			"name":   "structured_output",
			"strict": false,
			"schema": e.output,
		}
		parameters.Text = &responses.TextFormat{
			Format: format,
		}
	}

	finalOutput := []responses.InputMessageUnion{}

	// Main loop - driven by state machine
	for run.RunState.LoopIteration < e.maxLoops {
		// Drain queued input messages from the broker.
		if in.StreamID != "" && e.streamBroker != nil && !run.RunState.IsComplete() {
			queued, _ := e.streamBroker.DrainMessages(context.Background(), in.StreamID)
			run.AddMessagesToQueue(ctx, queued)
		}

		// Honor an external stop signal at iteration boundaries.
		// The caller (typically via ExecuteAsync's handle.Stop) sets a
		// flag on the broker for in.StreamID; we transition cleanly to
		// completed so the StepComplete branch persists state and emits
		// the run.completed event.
		if in.StreamID != "" && e.streamBroker != nil && !run.RunState.IsComplete() {
			stopped, _ := e.streamBroker.IsStopped(context.Background(), in.StreamID)
			if stopped {
				// The LLM may have already requested tool calls that we never
				// got to execute (pending immediate calls, or calls parked
				// awaiting approval). Their function_call messages are already
				// in history, so completing now would leave them without a
				// matching function_call_output — every subsequent turn on this
				// thread would then fail the provider's "each tool call needs a
				// result" invariant. Emit a synthetic cancelled result for each
				// so history stays consistent.
				cancelledCalls := append([]responses.FunctionCallMessage{}, run.RunState.PendingToolCalls...)
				cancelledCalls = append(cancelledCalls, run.RunState.ToolsAwaitingApproval...)
				for _, tc := range cancelledCalls {
					result := toolResponse(tc, toolCancelledBeforeExec)

					// Publish once (durable step) so streaming clients don't see
					// a dangling tool call either.
					e.durableStep.Do(func() {
						publish(&responses.ResponseChunk{
							OfFunctionCallOutput: result.FunctionCallOutputMessage,
						})
					})

					toolResultMsg := []responses.InputMessageUnion{
						{OfFunctionCallOutput: result.FunctionCallOutputMessage},
					}
					run.AddMessages(ctx, messages.New(in.Message.SenderID, toolResultMsg))
					finalOutput = append(finalOutput, toolResultMsg...)
				}

				// The stop notice is streamed as well as stored, so a client
				// watching the run sees the turn end rather than finding out
				// on the next reload. These are the same three chunks the LLM
				// emits for a message, so every translator downstream already
				// knows what to do with them. The id comes from the run, which
				// keeps it stable when a durable runtime replays the loop —
				// and matches the id history hands back afterwards.
				cancelID := "msg_" + runId + "-cancelled"
				cancelMsg := responses.InputMessageUnion{
					OfInputMessage: &responses.InputMessage{
						ID:   cancelID,
						Role: constants.RoleAssistant,
						Content: responses.InputContent{
							{OfOutputText: &responses.OutputTextContent{Text: runCancelledNotice}},
						},
					},
				}

				e.durableStep.Do(func() {
					item := responses.ChunkOutputItemData{
						Type: "message",
						Id:   cancelID,
						Role: constants.RoleAssistant,
					}
					publish(&responses.ResponseChunk{
						OfOutputItemAdded: &responses.ChunkOutputItem[constants.ChunkTypeOutputItemAdded]{Item: item},
					})
					publish(&responses.ResponseChunk{
						OfOutputTextDelta: &responses.ChunkOutputText[constants.ChunkTypeOutputTextDelta]{
							ItemId: cancelID,
							Delta:  runCancelledNotice,
						},
					})
					publish(&responses.ResponseChunk{
						OfOutputItemDone: &responses.ChunkOutputItem[constants.ChunkTypeOutputItemDone]{Item: item},
					})
				})

				run.AddMessages(ctx, messages.New(in.Message.SenderID, []responses.InputMessageUnion{cancelMsg}))
				finalOutput = append(finalOutput, cancelMsg)
				run.RunState.ToolsAwaitingApproval = nil
				run.RunState.TransitionToComplete()
			}
		}

		switch run.RunState.NextStep() {

		case agentstate.StepCallLLM:
			convMessages, err := run.GetMessages(ctx, e.Name)
			if err != nil {
				return &AgentOutput{Status: agentstate.RunStatusError, RunID: runId}, err
			}

			if reminder := budgetReminder(e.maxLoops - run.RunState.LoopIteration); reminder != nil {
				convMessages = append(convMessages, *reminder)
			}

			var activatedDeferredToolsDef []responses.ToolUnion
			if str, ok := run.State["activated_deferred_tools"]; ok {
				activatedToolNames := strings.Split(str, ",")
				for _, tool := range deferredTools {
					if t := tool.Tool(ctx); t.OfFunction != nil && slices.Contains(activatedToolNames, t.OfFunction.Name) {
						activatedDeferredToolsDef = append(activatedDeferredToolsDef, *t)
					}
				}
			}

			request := &responses.Request{
				Instructions: utils.Ptr(instruction),
				Input: responses.InputUnion{
					OfInputMessageList: convMessages,
				},
				Tools:      append(toolDefs, activatedDeferredToolsDef...),
				Parameters: parameters,
			}

			// The hooks see what the call is and what the run has spent, not
			// the prompt — see ModelCall. A hook that answers for the model
			// (an exhausted budget, say) supplies the reply and the provider
			// is never contacted.
			resp, err := RunWithModelCallHooks(ctx, e.modelCallHooks, &ModelCall{
				AgentName:     e.Name,
				Namespace:     in.Namespace,
				ThreadID:      in.ThreadID,
				SessionID:     in.SessionID,
				StreamID:      in.StreamID,
				RunID:         runId,
				RunContext:    in.RunContext,
				Model:         request.Model,
				LoopIteration: run.RunState.LoopIteration,
				ContextTokens: run.ContextTokens(),
				Usage:         run.RunState.Usage,
			}, func(callCtx context.Context) (*responses.Response, error) {
				// A stop lands mid-stream on the local runtime, where the
				// provider's request is this process's to cancel. The durable
				// runtimes cancel inside their own activity or step instead —
				// from here they hold a proxy, and cancelling that would end
				// the waiting rather than the work.
				streamCtx, cancel := StopCancelContext(callCtx, StopWatcherFrom(e.streamBroker), in.StreamID)
				defer cancel()
				return e.llm.NewStreamingResponses(streamCtx, request, publish)
			})
			if errors.Is(err, ErrModelCallStopped) {
				// Not a failure: the user stopped the run while the model was
				// still talking. Go back to the top of the loop, where the stop
				// check ends the run the same way it would have between
				// iterations — one cancellation path, not two.
				//
				// Whatever the model had said by then is dropped. It reached
				// the client as it streamed, but a stopped turn is recorded as
				// cancelled rather than as a half-answer the model never
				// finished and would be asked to continue from.
				continue
			}
			if err != nil {
				return &AgentOutput{Status: agentstate.RunStatusError, RunID: runId}, err
			}

			// Track the LLM's usage
			run.TrackUsage(resp.Usage)

			// Convert output to input messages and add to history
			inputMsgs := []responses.InputMessageUnion{}
			for _, outMsg := range resp.Output {
				inputMsg, err := outMsg.AsInput()
				if err != nil {
					slog.ErrorContext(ctx, "output msg conversion failed", slog.Any("error", err))
					return &AgentOutput{Status: agentstate.RunStatusError, RunID: runId}, err
				}
				inputMsgs = append(inputMsgs, inputMsg)
			}

			// AlreadyMeasured: TrackUsage above counted this reply against the
			// context window as part of the call's reported total, so
			// estimating it here would count it twice.
			run.AddMessages(ctx, messages.New(e.Name, inputMsgs), history.AlreadyMeasured())
			finalOutput = append(finalOutput, inputMsgs...)

			// Extract tool calls
			toolCalls := []responses.FunctionCallMessage{}
			for _, msg := range resp.Output {
				if msg.OfFunctionCall != nil {
					toolCalls = append(toolCalls, *msg.OfFunctionCall)
				}
			}

			if e.singleTurn {
				// Single turn: the model has responded, and that response — text
				// and/or tool calls, already in finalOutput — is the whole result.
				// Complete before any tool runs.
				run.RunState.TransitionToComplete()
			} else if len(toolCalls) == 0 {
				// No tools = done
				run.RunState.TransitionToComplete()
			} else {
				// Partition tools by approval requirement
				needsApproval, immediate := partitionByApproval(ctx, tools, toolCalls)

				// Execute immediate tools first (if any), then handle approval
				if len(immediate) > 0 {
					for _, tc := range immediate {
						run.RunState.QueuedApprovals = append(run.RunState.QueuedApprovals, tc.CallID)
					}
					run.RunState.TransitionToExecuteTools(immediate)
					// Store tools needing approval for after immediate execution
					if len(needsApproval) > 0 {
						run.RunState.ToolsAwaitingApproval = needsApproval
					}
				} else if len(needsApproval) > 0 {
					// Only approval-required tools, no immediate ones
					run.RunState.TransitionToAwaitApproval(needsApproval)
				}
			}

		case agentstate.StepExecuteTools:
			// Execute pending tool calls
			var handoffFn func() (*AgentOutput, error)
			var executableToolCalls []ExecutableToolCall

			// Flatten nested tool calls
			pendingToolCalls := []responses.FunctionCallMessage{}
			seenPausedToolCalls := map[string]bool{}
			for _, toolCall := range run.RunState.PendingToolCalls {
				parentToolCallID, isNestedTool := run.RunState.PendingNestedToolCalls[toolCall.CallID]
				if isNestedTool {
					if _, ok := seenPausedToolCalls[parentToolCallID]; !ok {
						pendingToolCalls = append(pendingToolCalls, run.RunState.PausedToolCalls[parentToolCallID])
					}
					seenPausedToolCalls[parentToolCallID] = true
				} else {
					pendingToolCalls = append(pendingToolCalls, toolCall)
				}
			}

			toolResults := make([]*ToolCallResponse, len(pendingToolCalls))
			for i, toolCall := range pendingToolCalls {
				if rejected := slices.Contains(run.RunState.QueuedRejections, toolCall.CallID); rejected {
					toolResults[i] = toolResponse(toolCall, "User has declined the request to call this tool")
					continue
				}

				if approved := slices.Contains(run.RunState.QueuedApprovals, toolCall.CallID); !approved {
					run.RunState.ToolsAwaitingApproval = append(run.RunState.ToolsAwaitingApproval, toolCall)
					continue
				}

				if toolCall.Name == "transfer_to_agent" {
					var param map[string]any
					if err := sonic.Unmarshal([]byte(toolCall.Arguments), &param); err != nil {
						return &AgentOutput{Status: agentstate.RunStatusError, RunID: runId}, err
					}

					for _, handoff := range e.handoffs {
						if handoff.Name == param["agent_name"] {
							toolResults[i] = toolResponse(toolCall, "Transferred to agent")

							handoffFn = func() (*AgentOutput, error) {
								return handoff.Agent.ExecuteWithRun(ctx, in, run)
							}
							break
						}
					}

					if toolResults[i] == nil {
						toolResults[i] = toolResponse(toolCall, "Failed to transfer to agent. Target agent not found")
					}
				} else {
					// Regular tool — queue for parallel execution
					tool := findTool(ctx, tools, toolCall.Name)
					if tool == nil {
						slog.ErrorContext(ctx, "tool not found", slog.String("tool_name", toolCall.Name))
						toolResults[i] = toolResponse(toolCall, "Tool not found: "+toolCall.Name)
						continue
					}

					_, resuming := run.RunState.PausedToolCalls[toolCall.CallID]

					var resumeMessages []responses.InputMessageUnion
					if resuming {
						approvedInner, rejectedInner := run.RunState.CollectNestedApprovalsForParent(toolCall.CallID)
						if len(approvedInner) > 0 || len(rejectedInner) > 0 {
							resolutions := make([]responses.InterruptResolution, 0, len(approvedInner)+len(rejectedInner))
							for _, id := range approvedInner {
								// Prefer the resolution the user actually submitted
								// (it carries mode-specific Content, e.g. a filled
								// form for a data elicitation); fall back to a bare
								// approve for plain approvals.
								if res, ok := run.RunState.Resolutions[id]; ok {
									resolutions = append(resolutions, res)
								} else {
									resolutions = append(resolutions, responses.InterruptResolution{CallID: id, Action: responses.InterruptActionApprove})
								}
							}
							for _, id := range rejectedInner {
								resolutions = append(resolutions, responses.InterruptResolution{CallID: id, Action: responses.InterruptActionReject})
							}
							resumeMessages = []responses.InputMessageUnion{
								{
									OfFunctionCallInterruptResolution: &responses.FunctionCallInterruptResolutionMessage{
										Resolutions: resolutions,
									},
								},
							}
						}
					}

					executableToolCalls = append(executableToolCalls, ExecutableToolCall{
						Index:    i,
						ToolName: toolCall.Name,
						Tool:     tool,
						ToolCall: &ToolCall{
							FunctionCallMessage: &toolCall,
							AgentName:           e.Name,
							Namespace:           in.Namespace,
							SessionID:           in.SessionID,
							ThreadID:            in.ThreadID,
							StreamID:            in.StreamID,
							RunContext:          in.RunContext,
							State:               run.State,
							ShouldResume:        resuming,
							ResumeMessages:      resumeMessages,
							Progress:            e.progressReporter(in.StreamID, toolCall.CallID, toolCall.Name),
						},
					})
				}
			}

			// Execute tools. Calls the stop unwound come back flagged
			// cancelled; the loop keeps history consistent for those and
			// ends the run at the next boundary, where the journaled stop
			// check decides.
			if len(executableToolCalls) > 0 {
				results := e.toolExecutor.ExecuteAll(ctx, executableToolCalls)

				for j, pe := range executableToolCalls {
					result := results[j]
					switch {
					case result.Err == nil && result.Response != nil:
						toolResults[pe.Index] = result.Response

						// If the tool response has interrupts process it
						if len(result.Response.Interrupts) > 0 {
							run.ProcessInterrupts(*pe.ToolCall.FunctionCallMessage, result.Response.Interrupts)
						}

					case result.Err != nil && ctx.Err() != nil:
						// The caller's context went away (not a user stop) —
						// abort the run.
						return &AgentOutput{Status: agentstate.RunStatusError, RunID: runId}, result.Err

					case result.Cancelled:
						// Record a synthetic result so the function_call
						// already in history keeps its matching output.
						slog.InfoContext(ctx, "tool execution cancelled by stop signal", slog.String("tool_name", pe.ToolCall.Name))
						toolResults[pe.Index] = toolResponse(*pe.ToolCall.FunctionCallMessage, toolCancelledDuringExec)

					case result.Err != nil:
						// Tool error — report to LLM as error result
						slog.ErrorContext(ctx, "tool execution failed", slog.String("tool_name", pe.ToolCall.Name), slog.Any("error", result.Err))
						toolResults[pe.Index] = toolResponse(*pe.ToolCall.FunctionCallMessage, fmt.Sprintf("Tool execution failed: %v", result.Err))

					default:
						// No response and no error — still needs an output to
						// keep the call/result pairing intact.
						slog.ErrorContext(ctx, "tool returned no response", slog.String("tool_name", pe.ToolCall.Name))
						toolResults[pe.Index] = toolResponse(*pe.ToolCall.FunctionCallMessage, "Tool execution failed: tool returned no response")
					}
				}
			}

			// Process all results in original order
			for _, toolResult := range toolResults {
				if toolResult == nil {
					continue
				}

				// Merge sub-agent context if present
				if toolResult.StateUpdates != nil {
					maps.Copy(run.State, toolResult.StateUpdates)
				}

				// Tool had interrupts, don't count it as completed
				if len(toolResult.Interrupts) > 0 {
					continue
				}

				// Durable step: publish the tool output chunk once, not on
				// every replay.
				e.durableStep.Do(func() {
					publish(&responses.ResponseChunk{
						OfFunctionCallOutput: toolResult.FunctionCallOutputMessage,
					})
				})

				toolResultMsg := []responses.InputMessageUnion{
					{OfFunctionCallOutput: toolResult.FunctionCallOutputMessage},
				}

				// Add tool result to history
				run.AddMessages(ctx, messages.New(in.Message.SenderID, toolResultMsg))
				finalOutput = append(finalOutput, toolResultMsg...)
			}

			run.RunState.ClearPendingTools()

			// Check if there are tools waiting for approval (queued during immediate execution)
			if run.RunState.HasToolsAwaitingApproval() {
				run.RunState.PromoteAwaitingToApproval()
			} else {
				run.RunState.TransitionToLLM()
			}

			if handoffFn != nil {
				return handoffFn()
			}

		case agentstate.StepAwaitApproval:
			err = run.SaveMessages(ctx)
			if err != nil {
				return &AgentOutput{Status: agentstate.RunStatusError, RunID: runId}, err
			}

			// Durable step: emit run.paused once, not on every replay.
			e.durableStep.Do(func() {
				e.runPaused(ctx, in.StreamID, runId, run.RunState)
			})

			return &AgentOutput{
				RunID:      runId,
				Status:     agentstate.RunStatusPaused,
				Interrupts: run.RunState.PendingInterrupts(),
			}, nil

		case agentstate.StepComplete:
			err = run.SaveMessages(ctx)
			if err != nil {
				return &AgentOutput{Status: agentstate.RunStatusError, RunID: runId}, err
			}

			// Durable step: emit run.completed once, not on every replay.
			e.durableStep.Do(func() {
				e.runCompleted(ctx, in.StreamID, runId, run.RunState)
			})

			return &AgentOutput{
				RunID:  runId,
				Status: agentstate.RunStatusCompleted,
				Output: finalOutput,
			}, nil
		}
	}

	// Max loops exceeded
	return &AgentOutput{Status: agentstate.RunStatusError, RunID: runId}, fmt.Errorf("exceeded maximum loops (%d)", e.maxLoops)
}

// publisher returns a function that publishes a chunk to the broker on
// the given stream channel. When the agent has no broker configured or
// no stream id, the returned function is a no-op so internal callers can
// invoke it unconditionally.
// budgetReminder returns an ephemeral developer-role message warning
// the agent that it is running out of iteration budget, or nil if the
// remaining budget is comfortable. The two-turn variant gives the
// agent room to wind down its work; the one-turn variant tells it to
// stop calling tools and produce a final answer immediately.
func budgetReminder(remaining int) *responses.InputMessageUnion {
	var text string
	switch remaining {
	case 2:
		text = "You have 2 turns remaining in this run. Start wrapping up: finish any in-flight work and prepare a final answer to the user."
	case 1:
		text = "This is your last allowed turn. Do not call any more tools. Provide a final answer to the user now using whatever information you have gathered."
	default:
		return nil
	}
	return &responses.InputMessageUnion{
		OfInputMessage: &responses.InputMessage{
			Role: constants.RoleUser,
			Content: responses.InputContent{
				{OfInputText: &responses.InputTextContent{Text: text}},
			},
		},
	}
}

func (e *Agent) publisher(streamID string) func(chunk *responses.ResponseChunk) {
	if e.streamBroker == nil || streamID == "" {
		return func(*responses.ResponseChunk) {}
	}
	broker := e.streamBroker
	return func(chunk *responses.ResponseChunk) {
		_ = broker.Publish(context.Background(), streamID, chunk)
	}
}

func (e *Agent) runCreated(ctx context.Context, streamID, runId, traceId string) {
	publish := e.publisher(streamID)
	publish(&responses.ResponseChunk{
		OfRunCreated: &responses.ChunkRun[constants.ChunkTypeRunCreated]{
			RunState: responses.ChunkRunData{
				Id:      runId,
				Object:  "run",
				Status:  "created",
				TraceID: traceId,
			},
		},
	})

	publish(&responses.ResponseChunk{
		OfRunInProgress: &responses.ChunkRun[constants.ChunkTypeRunInProgress]{
			RunState: responses.ChunkRunData{
				Id:      runId,
				Object:  "run",
				Status:  "in_progress",
				TraceID: traceId,
			},
		},
	})
}

func (e *Agent) runPaused(ctx context.Context, streamID, runId string, runState *agentstate.RunState) {
	e.publisher(streamID)(&responses.ResponseChunk{
		OfRunPaused: &responses.ChunkRun[constants.ChunkTypeRunPaused]{
			RunState: responses.ChunkRunData{
				Id:                runId,
				Object:            "run",
				Status:            "paused",
				PendingInterrupts: runState.PendingInterrupts(),
				Usage:             runState.Usage,
				TraceID:           runState.TraceID,
			},
		},
	})
}

func (e *Agent) runCompleted(ctx context.Context, streamID, runId string, runState *agentstate.RunState) {
	e.publisher(streamID)(&responses.ResponseChunk{
		OfRunCompleted: &responses.ChunkRun[constants.ChunkTypeRunCompleted]{
			RunState: responses.ChunkRunData{
				Id:      runId,
				Object:  "run",
				Status:  "completed",
				Usage:   runState.Usage,
				TraceID: runState.TraceID,
			},
		},
	})
}

// AgentHandle is returned by Execute. The caller has two valid usage
// patterns: drain Chunks yourself (and optionally call Wait for the
// aggregated AgentOutput), or call Result to drain + aggregate in one
// step. Mixing the two — calling Wait without draining Chunks — risks
// deadlock because the broker's Publish back-pressures when no
// subscriber is reading.
type AgentHandle struct {
	StreamID string
	Chunks   <-chan *responses.ResponseChunk

	broker StreamBroker
	done   chan struct{}
	result *AgentOutput
	err    error
}

// Stop signals the agent to stop and transition to completed state.
//
// A tool call already running has its context cancelled, and is abandoned
// after a grace period if it ignores that — it keeps running in the
// background, unobserved. Each cancelled call gets a synthetic result so
// history keeps its call/result pairing. An in-flight LLM call always
// finishes streaming, so a stop during one lands at the next boundary.
//
// Reaching a running tool needs a broker implementing StopWatcher (the
// memory and Redis brokers do); the tool wrapper watches that flag
// wherever the tool runs. With a plain broker the stop still lands, at
// the loop's next iteration boundary.
//
// Use Wait or Result to block until the run finishes.
func (h *AgentHandle) Stop(ctx context.Context) error {
	return h.broker.Stop(ctx, h.StreamID)
}

// EnqueueMessage pushes msg onto the run's broker queue. The agent
// drains the queue at iteration boundaries (alongside the IsStopped
// check) and folds queued messages into the current run via
// ProcessIncomingMessages — approval responses become approve/reject
// queues; other input messages slot into the next LLM call.
//
// Use this to deliver follow-ups (user messages, approval/rejection
// decisions) to a run that's still in flight. For runs that have
// already paused and exited, the next agent.Execute on the same
// thread is the right entry instead.
func (h *AgentHandle) EnqueueMessage(ctx context.Context, msg history.Message) error {
	return h.broker.EnqueueMessage(ctx, h.StreamID, msg)
}

// Wait blocks until the run finishes and returns the aggregated output.
// It is the lower-level counterpart to Result and is safe to call only
// after the chunk channel has been drained — calling Wait while the
// broker still has buffered chunks pending will deadlock.
func (h *AgentHandle) Wait() (*AgentOutput, error) {
	<-h.done
	return h.result, h.err
}

// Result drains the chunk channel (discarding chunks) and then returns
// the aggregated AgentOutput. Use this when you only care about the
// final output and don't need to observe streaming chunks. To consume
// chunks yourself, range over Chunks and then call Wait instead.
func (h *AgentHandle) Result() (*AgentOutput, error) {
	for range h.Chunks {
	}
	return h.Wait()
}

func toolResponse(toolCall responses.FunctionCallMessage, textOutput string) *ToolCallResponse {
	return &ToolCallResponse{
		FunctionCallOutputMessage: &responses.FunctionCallOutputMessage{
			ID:     toolCall.ID,
			CallID: toolCall.CallID,
			Output: responses.FunctionCallOutputContentUnion{
				OfString: utils.Ptr(textOutput),
			},
		},
	}
}

// stickyHandoffTarget resolves the thread's last-active agent name to a
// concrete agent reachable from this one through the handoff graph.
// Returns nil when the name is empty, is this agent itself, or isn't a
// reachable handoff target (e.g. handoffs were reconfigured since the
// last turn) — in every such case the caller falls back to running this
// agent normally.
func (e *Agent) stickyHandoffTarget(name string) *Agent {
	if name == "" || name == e.Name {
		return nil
	}
	return e.findHandoffAgent(name, map[string]bool{})
}

// findHandoffAgent searches this agent's handoff graph (depth-first,
// cycle-safe) for an agent whose name — or the name it was registered
// under — matches. Only *Agent handoff targets are considered.
func (e *Agent) findHandoffAgent(name string, visited map[string]bool) *Agent {
	if visited[e.Name] {
		return nil
	}
	visited[e.Name] = true

	for _, h := range e.handoffs {
		target, ok := h.Agent.(*Agent)
		if !ok {
			continue
		}
		if h.Name == name || target.Name == name {
			return target
		}
		if found := target.findHandoffAgent(name, visited); found != nil {
			return found
		}
	}
	return nil
}
