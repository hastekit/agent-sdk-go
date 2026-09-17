package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/google/uuid"
	"github.com/hastekit/agent-sdk-go/pkg/agents/agentstate"
	"github.com/hastekit/agent-sdk-go/pkg/agents/messages"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
)

// A2A exposes an agent using the A2A 1.0 JSON-RPC binding. Retain the adapter
// across requests: its default task store and execution manager are in memory.
type A2A struct {
	agent            *Agent
	namespace        string
	handlerOptions   []a2asrv.RequestHandlerOption
	InvokeHandler    http.Handler
	AgentCardHandler http.Handler
}

type A2AOption func(*A2A)

// WithA2ANamespace binds an adapter to a host-resolved namespace. Client metadata
// and A2A tenant parameters cannot override it. Use separate adapters per namespace.
func WithA2ANamespace(namespace string) A2AOption {
	return func(a *A2A) {
		if strings.TrimSpace(namespace) != "" {
			a.namespace = namespace
		}
	}
}

// WithA2AHandlerOptions configures the upstream task store, queues and limits.
// Stores and queues must be isolated to this adapter's agent and namespace.
func WithA2AHandlerOptions(opts ...a2asrv.RequestHandlerOption) A2AOption {
	return func(a *A2A) { a.handlerOptions = append(a.handlerOptions, opts...) }
}

func (a *Agent) A2A(card *a2a.AgentCard, opts ...A2AOption) *A2A {
	adapter := &A2A{agent: a, namespace: "default"}
	for _, opt := range opts {
		opt(adapter)
	}
	adapter.handlerOptions = append(adapter.handlerOptions, a2asrv.WithCallInterceptors(a2aNamespaceIdentity{namespace: adapter.namespace}))
	handler := a2asrv.NewHandler(adapter, adapter.handlerOptions...)
	adapter.InvokeHandler = a2asrv.NewJSONRPCHandler(handler)
	adapter.AgentCardHandler = a2asrv.NewStaticAgentCardHandler(card)
	return adapter
}

// A2A context IDs identify conversations. Include the agent identity so two
// agents sharing conversation persistence cannot read each other's threads.
func (a *A2A) threadID(contextID string) string {
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte("hastekit:a2a:"+a.agent.Name+"\x00"+contextID)).String()
}
func (a *A2A) streamID(ec *a2asrv.ExecutorContext) string {
	messageID := ""
	if ec.Message != nil {
		messageID = ec.Message.ID
	} else if ec.StoredTask != nil {
		for i := len(ec.StoredTask.History) - 1; i >= 0; i-- {
			message := ec.StoredTask.History[i]
			if message != nil && message.Role == a2a.MessageRoleUser {
				messageID = message.ID
				break
			}
		}
	}
	// Each resumed execution needs a fresh broker channel; completed channels
	// retain their old transcript and cannot be reused without resetting them.
	return StreamIDForTask(a.namespace, a.threadID(ec.ContextID), string(ec.TaskID)+"\x00"+messageID)
}

func (a *A2A) Execute(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		input, err := a.input(ec)
		if err != nil {
			yield(nil, fmt.Errorf("%w: %v", a2a.ErrInvalidParams, err))
			return
		}
		if ec.StoredTask == nil && !yield(a2a.NewSubmittedTask(ec, ec.Message), nil) {
			return
		}
		if !yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateWorking, nil), nil) {
			return
		}
		fail := func(err error) {
			slog.ErrorContext(ctx, "A2A execution failed", "agent", a.agent.Name, "task_id", ec.TaskID, "error", err)
			yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateFailed, a2a.NewMessageForTask(a2a.MessageRoleAgent, ec, a2a.NewTextPart("Agent execution failed."))), nil)
		}
		handle, err := a.agent.Execute(ctx, input)
		if err != nil {
			fail(err)
			return
		}
		// The agent handle detaches execution from HTTP subscriptions. The A2A SDK
		// owns task lifetime; cancellation of its executor must also stop remote work.
		stop := func() {
			stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			if err := handle.Stop(stopCtx); err != nil {
				slog.ErrorContext(stopCtx, "A2A stop failed", "error", err)
			}
		}
		stopWatch := context.AfterFunc(ctx, stop)
		defer stopWatch()
		var artifactID a2a.ArtifactID
		for {
			select {
			case <-ctx.Done():
				return
			case chunk, ok := <-handle.Chunks:
				if !ok {
					goto finished
				}
				if chunk == nil || chunk.OfOutputTextDelta == nil || chunk.OfOutputTextDelta.Delta == "" {
					continue
				}
				event := a2a.NewArtifactEvent(ec, a2a.NewTextPart(chunk.OfOutputTextDelta.Delta))
				if artifactID == "" {
					artifactID = event.Artifact.ID
				} else {
					event.Artifact.ID = artifactID
					event.Append = true
				}
				if !yield(event, nil) {
					stop()
					return
				}
			}
		}
	finished:
		output, err := handle.Wait(ctx)
		if err != nil {
			if ctx.Err() == nil {
				fail(err)
			}
			return
		}
		if output == nil {
			fail(fmt.Errorf("agent returned no output"))
			return
		}
		if output.Status == agentstate.RunStatusError {
			fail(fmt.Errorf("agent returned error status"))
			return
		}
		if text := output.Text(); text != "" || artifactID != "" {
			part := a2a.NewTextPart(text)
			if a.agent.output != nil && json.Valid([]byte(text)) {
				var data any
				if json.Unmarshal([]byte(text), &data) == nil {
					part = a2a.NewDataPart(data)
				}
			}
			event := a2a.NewArtifactEvent(ec, part)
			if artifactID != "" {
				event.Artifact.ID = artifactID
			}
			// Replace provisional deltas with the authoritative final output. This also
			// handles runtimes that only publish their completed response.
			event.LastChunk = true
			if !yield(event, nil) {
				return
			}
		}
		state := a2a.TaskStateCompleted
		var message *a2a.Message
		if output.Status == agentstate.RunStatusPaused {
			state = a2a.TaskStateInputRequired
			data, err := json.Marshal(output.Interrupts)
			if err != nil {
				fail(err)
				return
			}
			var interrupts any
			if err := json.Unmarshal(data, &interrupts); err != nil {
				fail(err)
				return
			}
			message = a2a.NewMessageForTask(a2a.MessageRoleAgent, ec, a2a.NewDataPart(map[string]any{"type": "hastekit.interrupts", "interrupts": interrupts}))
		}
		event := a2a.NewStatusUpdateEvent(ec, state, message)
		event.Metadata = map[string]any{"hastekit.run_id": output.RunID}
		yield(event, nil)
	}
}

func (a *A2A) Cancel(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		if err := a.agent.Stop(ctx, a.streamID(ec)); err != nil {
			yield(nil, err)
			return
		}
		yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCanceled, nil), nil)
	}
}

func (a *A2A) input(ec *a2asrv.ExecutorContext) (*AgentInput, error) {
	if ec.Message == nil || ec.Message.Role != a2a.MessageRoleUser || len(ec.Message.Parts) == 0 {
		return nil, fmt.Errorf("a nonempty user message is required")
	}
	var input []responses.InputMessageUnion
	for _, part := range ec.Message.Parts {
		if part == nil {
			return nil, fmt.Errorf("message contains a null part")
		}
		var text string
		switch content := part.Content.(type) {
		case a2a.Text:
			text = string(content)
		case a2a.Data:
			data, err := json.Marshal(content.Value)
			if err != nil {
				return nil, fmt.Errorf("invalid data part: %w", err)
			}
			var envelope struct {
				Type        string                          `json:"type"`
				Resolutions []responses.InterruptResolution `json:"resolutions"`
			}
			_ = json.Unmarshal(data, &envelope)
			if envelope.Type == "hastekit.interrupt_response" {
				if ec.StoredTask == nil || ec.StoredTask.Status.State != a2a.TaskStateInputRequired || len(envelope.Resolutions) == 0 {
					return nil, fmt.Errorf("interrupt responses require a task awaiting input")
				}
				for _, resolution := range envelope.Resolutions {
					if resolution.CallID == "" || (resolution.Action != "approve" && resolution.Action != "reject") {
						return nil, fmt.Errorf("interrupt response requires call_id and approve/reject action")
					}
				}
				input = append(input, responses.InputMessageUnion{OfFunctionCallInterruptResolution: &responses.FunctionCallInterruptResolutionMessage{ID: ec.Message.ID, Resolutions: envelope.Resolutions}})
				continue
			}
			text = string(data)
		default:
			return nil, fmt.Errorf("only text and JSON data parts are supported")
		}
		input = append(input, responses.InputMessageUnion{OfEasyInput: &responses.EasyMessage{Role: constants.RoleUser, Content: responses.EasyInputContentUnion{OfString: &text}}})
	}
	in := &AgentInput{Namespace: a.namespace, ThreadID: a.threadID(ec.ContextID), StreamID: a.streamID(ec), Message: messages.NewWithID(ec.Message.ID, "a2a", input), RunContext: map[string]any{"A2A": map[string]any{"task_id": ec.TaskID, "context_id": ec.ContextID, "metadata": ec.Metadata}}}

	headers := map[string]string{}
	if ec.ServiceParams != nil {
		for key, values := range ec.ServiceParams.List() {
			key = http.CanonicalHeaderKey(key)
			if len(values) > 0 && (strings.HasPrefix(key, "X-") || key == "Authorization") {
				headers[strings.ReplaceAll(key, "-", "_")] = values[0]
			}
		}
	}
	in.RunContext["Header"] = headers
	if ec.StoredTask != nil {
		if runID, ok := ec.StoredTask.Metadata["hastekit.run_id"].(string); ok {
			in.PreviousRunID = runID
		}
	}
	if raw, ok := ec.Message.Metadata["hastekit.skills"]; ok {
		data, err := json.Marshal(raw)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(data, &in.Skills); err != nil {
			return nil, fmt.Errorf("invalid hastekit.skills: %w", err)
		}
	}
	return in, nil
}

var _ a2asrv.AgentExecutor = (*A2A)(nil)

// Task ownership follows the host-resolved namespace, matching the AG-UI APIs.
// This supplies the SDK's task-list identity; authentication remains with the host.
type a2aNamespaceIdentity struct {
	a2asrv.PassthroughCallInterceptor
	namespace string
}

func (i a2aNamespaceIdentity) Before(ctx context.Context, call *a2asrv.CallContext, _ *a2asrv.Request) (context.Context, any, error) {
	call.User = a2asrv.NewAuthenticatedUser(i.namespace, nil)
	return ctx, nil, nil
}
