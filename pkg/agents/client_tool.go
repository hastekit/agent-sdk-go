package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
)

// A client tool is one the client runs itself — a browser action such as
// reading the current page or opening a dialog. The client sends its
// definitions with a run (AG-UI RunAgentInput.tools, CopilotKit's
// useFrontendTool), the model calls it like any other tool, and the client
// computes the result. The client runs every call to one of its own tools as
// soon as it sees it and sends the result as an ordinary tool message on the
// thread.
//
// A tool message that reaches a live run is kept for its call by the stream
// broker rather than queued for the loop. A client tool waits there for its
// result (StreamBroker.WaitToolResult), up to ClientToolOptions.Timeout, so the run
// carries on without a second request. If the result has not arrived by then,
// the run pauses on the call with a client_tool interrupt, and a tool message
// sent after the pause starts the run that resumes it — which is also what
// stock AG-UI clients such as CopilotKit send, after the run has ended.

// ClientToolDefinition is one tool a client offers for a run.
type ClientToolDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

// DefaultClientToolTimeout is how long a client tool call waits for the
// client's result, when no timeout is configured, before the run pauses on it.
const DefaultClientToolTimeout = 30 * time.Second

// maxClientTools bounds how many tools one run may add.
const maxClientTools = 128

var clientToolName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ClientToolOptions configures an agent's client tools. The zero value is ready to use.
type ClientToolOptions struct {
	// Timeout bounds how long a client tool call waits for the client's result
	// before the run pauses on it. Defaults to DefaultClientToolTimeout. A
	// negative timeout pauses at once, for clients that only answer after the
	// run has paused.
	Timeout time.Duration
}

// timeout is how long a call waits: zero for a client that answers only after a pause.
func (o ClientToolOptions) timeout() time.Duration {
	switch {
	case o.Timeout < 0:
		return 0
	case o.Timeout == 0:
		return DefaultClientToolTimeout
	default:
		return o.Timeout
	}
}

// clientTool is the server-side stand-in for one client tool in one run.
type clientTool struct {
	*BaseTool
	options ClientToolOptions
	broker  StreamBroker
}

// result reads the client's output for call from the broker, waiting up to
// timeout. Without a stream there is nowhere for one to arrive.
func (t *clientTool) result(ctx context.Context, call *ToolCall, timeout time.Duration) (string, bool, error) {
	if t.broker == nil || call.StreamID == "" {
		return "", false, nil
	}
	return t.broker.WaitToolResult(ctx, call.StreamID, call.CallID, timeout)
}

func (t *clientTool) Execute(ctx context.Context, call *ToolCall) (*ToolCallResponse, error) {
	if call == nil || call.FunctionCallMessage == nil {
		return nil, errors.New("client tool requires a tool call")
	}

	// A resumed call carries the client's answer as its resolution — the
	// content of the tool message that resumed it — or the client sent it
	// while the paused run was still finishing. A resolution without content
	// is not an answer: the run resumed for something else (an approval the
	// same pause was holding), so the call pauses again until the client
	// answers. A rejected call never reaches here; the loop answers it.
	if call.ShouldResume {
		if res, ok := clientToolResolution(call); ok && len(res.Content) > 0 {
			return toolResponse(*call.FunctionCallMessage, ClientToolOutput(res.Content)), nil
		}
		if result, found, err := t.result(ctx, call, 0); err == nil && found {
			return toolResponse(*call.FunctionCallMessage, result), nil
		}
		return clientToolPause(call), nil
	}

	// Wait for the client's result while the run is still going.
	result, found, err := t.result(ctx, call, t.options.timeout())
	switch {
	case err == nil && found:
		return toolResponse(*call.FunctionCallMessage, result), nil
	case err != nil && ctx.Err() != nil:
		// Stopped while waiting: the loop records the call as cancelled.
		return nil, err
	case err != nil:
		// The wait is only the fast path, so a failed one pauses as if the
		// client had not answered in time.
		slog.WarnContext(ctx, "waiting for a client tool result failed; pausing", slog.String("tool", call.Name), slog.Any("error", err))
	}

	// No result in time: pause until the client answers on a later request.
	return clientToolPause(call), nil
}

// clientToolPause pauses the run on call until the client answers it.
func clientToolPause(call *ToolCall) *ToolCallResponse {
	return &ToolCallResponse{Interrupts: []responses.Interrupt{{
		FunctionCallMessage: *call.FunctionCallMessage,
		Mode:                responses.InterruptModeClientTool,
	}}}
}

// clientToolResolution finds the answer for this call among the resume messages.
func clientToolResolution(call *ToolCall) (responses.InterruptResolution, bool) {
	for _, msg := range call.ResumeMessages {
		if msg.OfFunctionCallInterruptResolution == nil {
			continue
		}
		for _, res := range msg.OfFunctionCallInterruptResolution.Resolutions {
			if res.CallID == call.CallID {
				return res, true
			}
		}
	}
	return responses.InterruptResolution{}, false
}

// ClientToolOutput renders a client's result for the model: a JSON string is
// used as text, and any other JSON value is passed through as JSON text.
func ClientToolOutput(content json.RawMessage) string {
	if len(content) == 0 {
		return ""
	}
	var text string
	if err := json.Unmarshal(content, &text); err == nil {
		return text
	}
	return string(content)
}

// clientToolStandIn answers a resumed client tool call for a tool the run does
// not offer. It is never shown to the model; it only resolves the paused call.
func (e *Agent) clientToolStandIn(name string) Tool {
	return &clientTool{
		BaseTool: &BaseTool{Name: name, ToolUnion: responses.ToolUnion{OfFunction: &responses.FunctionTool{Name: name}}},
		options:  e.clientTools,
		broker:   e.streamBroker,
	}
}

// prepareClientTools adds the run's client tools after every server-side tool,
// so a client can never shadow or replace a tool the developer configured.
func (e *Agent) prepareClientTools(ctx context.Context, in *AgentInput, tools []Tool) ([]Tool, error) {
	if len(in.ClientTools) == 0 {
		return tools, nil
	}
	if len(in.ClientTools) > maxClientTools {
		return nil, fmt.Errorf("a run may offer at most %d client tools", maxClientTools)
	}
	taken := map[string]bool{}
	for _, tool := range tools {
		taken[functionName(tool)] = true
	}

	result := append([]Tool(nil), tools...)
	for _, def := range in.ClientTools {
		if !clientToolName.MatchString(def.Name) {
			return nil, fmt.Errorf("invalid client tool name %q", def.Name)
		}
		if taken[def.Name] {
			slog.WarnContext(ctx, "client tool ignored: name is taken by a server tool", slog.String("tool", def.Name))
			continue
		}
		taken[def.Name] = true
		parameters := def.Parameters
		if parameters == nil {
			parameters = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		result = append(result, &clientTool{
			BaseTool: &BaseTool{
				Name: def.Name,
				ToolUnion: responses.ToolUnion{OfFunction: &responses.FunctionTool{
					Name:        def.Name,
					Description: utils.Ptr(def.Description),
					Parameters:  parameters,
					Strict:      utils.Ptr(false),
				}},
			},
			options: e.clientTools,
			broker:  e.streamBroker,
		})
	}
	return result, nil
}
