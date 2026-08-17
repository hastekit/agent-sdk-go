package mcpclient

import (
	"fmt"

	"github.com/bytedance/sonic"
	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Elicitation is how an MCP server asks the *user* for something mid-call: a
// form to fill in, or a URL to visit.
//
// A server asks by returning it from the tool call (SEP-2322). The result
// carries InputRequests instead of content, along with an opaque RequestState,
// and the client answers by calling the tool again with InputResponses and
// that state echoed back.
//
// That two-call shape is already the agent's pause. The first call ends the
// run with an interrupt; the answer arrives on a later request, from a person,
// after the state has been persisted. So this drives the protocol directly and
// turns off the client's automatic multi-round-trip middleware (see
// transport.go) — that middleware exists to answer an input request from a
// callback and retry immediately, and at that moment there is nobody to ask.
// Turning it off is also why no ElicitationHandler is registered: on this
// protocol version the handler is only that middleware's callback.
//
// The request id and RequestState are carried across the pause in thread
// state, so the resuming call answers the server's original question instead
// of provoking a fresh one — which is what lets a server resume from where it
// left off rather than redo the work that preceded the question.

// elicitationStateKey namespaces the per-call record in thread state. Thread
// state survives a durable replay, which is the same guarantee the pause
// itself has.
const elicitationStateKey = "mcp/elicitation/"

// pendingElicitation is what has to outlive the pause: which request to
// answer, and the server's own state to hand back with it.
type pendingElicitation struct {
	ID           string `json:"id"`
	RequestState string `json:"request_state,omitempty"`
}

// elicitationPause turns a tool result that asks for input into the pause the
// agent loop understands. It returns (nil, nil) for an ordinary result.
func elicitationPause(params *agents.ToolCall, res *mcp.CallToolResult) (*agents.ToolCallResponse, error) {
	if res == nil || !res.NeedsInput() {
		return nil, nil
	}
	if len(res.InputRequests) == 0 {
		// The server is shedding load: it wants the call retried later but
		// has nothing to ask. Nothing to pause on.
		return nil, fmt.Errorf("mcp server asked for input without saying what it needs")
	}
	if len(res.InputRequests) > 1 {
		// One interrupt carries one answer, so a second question in the same
		// call would have nowhere to put its reply.
		return nil, fmt.Errorf("mcp tool raised %d input requests in one call; only one is supported", len(res.InputRequests))
	}

	var id string
	var elicit *mcp.ElicitParams
	for requestID, request := range res.InputRequests {
		id = requestID
		p, ok := request.(*mcp.ElicitParams)
		if !ok {
			// Sampling and roots can also arrive here. Neither is something a
			// user answers, so neither maps onto a run pause.
			return nil, fmt.Errorf("mcp tool asked for %T, which this client does not fulfil", request)
		}
		elicit = p
	}

	record, err := sonic.MarshalString(pendingElicitation{ID: id, RequestState: res.RequestState})
	if err != nil {
		return nil, fmt.Errorf("record elicitation state: %w", err)
	}

	mode := responses.InterruptModeForm
	if elicit.Mode == "url" || (elicit.Mode == "" && elicit.URL != "") {
		mode = responses.InterruptModeURL
	}

	return &agents.ToolCallResponse{
		Interrupts: []responses.Interrupt{{
			FunctionCallMessage: *params.FunctionCallMessage,
			Mode:                mode,
			Elicitations:        []mcp.ElicitParams{*elicit},
		}},
		StateUpdates: map[string]string{elicitationStateKey + params.CallID: record},
	}, nil
}

// resumeElicitation fills in the user's answer before a resumed call goes out,
// so the server receives the reply to the question it asked, together with the
// state it issued alongside it. It is a no-op for a call that is not resuming
// or that paused for something else.
func resumeElicitation(callParams *mcp.CallToolParams, params *agents.ToolCall) {
	if callParams == nil || params == nil || !params.ShouldResume {
		return
	}
	record := params.State[elicitationStateKey+params.CallID]
	if record == "" {
		return
	}
	var pending pendingElicitation
	if err := sonic.UnmarshalString(record, &pending); err != nil || pending.ID == "" {
		return
	}
	answer := answerFromResume(params)
	if answer == nil {
		return
	}

	callParams.InputResponses = mcp.InputResponseMap{pending.ID: answer}
	callParams.RequestState = pending.RequestState
}

// answerFromResume pulls the user's reply out of the resolutions the agent
// loop delivered, and translates it into MCP's vocabulary: an approval
// carrying form data is an "accept", a rejection is a "decline".
func answerFromResume(params *agents.ToolCall) *mcp.ElicitResult {
	if params == nil || !params.ShouldResume {
		return nil
	}

	var fallback *responses.InterruptResolution
	for _, msg := range params.ResumeMessages {
		if msg.OfFunctionCallInterruptResolution == nil {
			continue
		}
		for _, res := range msg.OfFunctionCallInterruptResolution.Resolutions {
			// Prefer the resolution for this exact call. A tool that raised
			// its own elicitation resolves under its own call id; the
			// fallback covers a resolution that arrived without one.
			if params.FunctionCallMessage != nil && res.CallID == params.CallID {
				return elicitResult(res)
			}
			if fallback == nil {
				fallback = &res
			}
		}
	}
	if fallback != nil {
		return elicitResult(*fallback)
	}
	return nil
}

func elicitResult(res responses.InterruptResolution) *mcp.ElicitResult {
	if res.Action != responses.InterruptActionApprove {
		return &mcp.ElicitResult{Action: "decline"}
	}

	out := &mcp.ElicitResult{Action: "accept"}
	if len(res.Content) > 0 {
		// A malformed body is dropped rather than failing the resume: the
		// server validates the content against its own schema and will say
		// so far more usefully than a decode error here.
		var content map[string]any
		if err := sonic.Unmarshal(res.Content, &content); err == nil {
			out.Content = content
		}
	}
	return out
}
