package agents

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/hastekit/agent-sdk-go/pkg/genai"
	"go.opentelemetry.io/otel/attribute"
)

// StreamBufferSize bounds the pending event backlog for one handle.
const StreamBufferSize = 256

// ErrStreamOverflow reports an incomplete event stream; the final output remains available.
var ErrStreamOverflow = errors.New("agent stream buffer overflow: event stream is incomplete")

// Input is an alias for the common execution input.
type Input = AgentInput

// Run executes synchronously without subscribing to events. Canceling ctx
// cancels local I/O and signals remote execution through the broker.
func (e *Agent) Run(ctx context.Context, in *AgentInput) (result *AgentOutput, err error) {
	input, err := e.prepareInput(ctx, in)
	if err != nil {
		return nil, err
	}
	stopped := make(chan error, 1)
	stopWatch := context.AfterFunc(ctx, func() {
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		stopped <- e.streamBroker.Stop(stopCtx, input.StreamID)
	})
	defer func() {
		if !stopWatch() {
			err = errors.Join(err, <-stopped)
		}
		if ctx.Err() != nil {
			err = errors.Join(ctx.Err(), err)
		}
	}()
	return e.runExecution(ctx, input)
}

// Execute starts a detached execution with an event subscription. Canceling ctx
// disconnects the subscription; use Stop to stop the execution.
func (e *Agent) Execute(ctx context.Context, in *AgentInput) (*AgentHandle, error) {
	input, err := e.prepareInput(ctx, in)
	if err != nil {
		return nil, err
	}
	h, closeSubscription, err := e.subscribe(ctx, input.StreamID)
	if err != nil {
		return nil, err
	}
	go func() {
		defer close(h.done)
		defer closeSubscription()
		h.result, h.err = e.runExecution(context.WithoutCancel(ctx), input)
	}()
	return h, nil
}

func (e *Agent) prepareInput(ctx context.Context, in *AgentInput) (*AgentInput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if in == nil {
		return nil, fmt.Errorf("agent input is nil")
	}
	if e.streamBroker == nil {
		return nil, fmt.Errorf("execution requires a stream broker")
	}
	input := *in
	input.Skills.Enable = append([]string(nil), in.Skills.Enable...)
	input.Skills.Disable = append([]string(nil), in.Skills.Disable...)
	if input.StreamID == "" {
		input.StreamID = uuid.NewString()
	}
	if input.ThreadID == "" {
		input.ThreadID = uuid.NewString()
	}
	if input.Message.ID == "" {
		input.Message.ID = uuid.NewString()
	}
	return &input, nil
}

func (e *Agent) runExecution(ctx context.Context, in *AgentInput) (*AgentOutput, error) {
	// GenAI invoke_agent span for the whole run. Callers that don't want
	// this span invoke ExecuteWithoutTrace directly instead.
	runCtx, span := tracer.Start(ctx, genai.OpInvokeAgent+" "+e.Name)
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
	result, err := e.ExecuteWithoutTrace(runCtx, in)

	if result != nil {
		if s, ok := genai.OutputMessages(result.Output); ok {
			span.SetAttributes(attribute.String(genai.AttrOutputMessages, s))
		}
	}
	return result, err
}

// Text concatenates assistant output text in order, excluding reasoning and tool
// messages. It is safe for nil, empty and multimodal results.
func (o *AgentOutput) Text() string {
	if o == nil {
		return ""
	}
	var b strings.Builder
	for _, message := range o.Output {
		if message.OfOutputMessage == nil || message.OfOutputMessage.Content == nil {
			continue
		}
		for _, content := range *message.OfOutputMessage.Content {
			if content.OfOutputText != nil {
				b.WriteString(content.OfOutputText.Text)
			}
		}
	}
	return b.String()
}
