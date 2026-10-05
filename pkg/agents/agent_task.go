package agents

import (
	"context"
	"fmt"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/agents/agentstate"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
)

// agentTaskPollInterval is how often a retried wait looks again at a run an
// earlier attempt started.
var agentTaskPollInterval = time.Second

// AgentTaskOutcome is what became of a message a background task handed to an
// agent.
type AgentTaskOutcome struct {
	// Joined is set when the message joined a run that something else
	// started, rather than starting one: it is answered in that run, and
	// whoever started the run reports the answer.
	Joined bool

	// Output is the run the message started, once it ended (completed,
	// paused or failed). Nil when Joined.
	Output *AgentOutput
}

// RunAgentTask runs input — a message to agent on input.ThreadID, sent by a
// background task's wait — so that the wait is safe to retry. A durable
// runtime retries a wait whose worker died, and the message may already be in
// the thread by then:
//
//   - it opened a run that has ended: that run's answer is returned, and the
//     turn is not run again;
//   - it opened a run still going: the wait waits for that run and returns its
//     answer, rather than starting a second;
//   - it went into a run something else opened: it is reported as Joined
//     again;
//   - it is not there: it is delivered as a turn arrives — joining the run
//     going on the thread's stream, or claiming the stream and starting one.
//
// input.Message.ID must be fixed by the task, so a retry carries the same id.
// input.StreamID must be the thread's channel (see StreamIDForThread).
func RunAgentTask(ctx context.Context, agent *Agent, input *AgentInput) (AgentTaskOutcome, error) {
	for {
		row, opened, err := findAgentTaskMessage(ctx, agent, input)
		if err != nil {
			return AgentTaskOutcome{}, err
		}
		if row == nil {
			break
		}
		if !opened {
			return AgentTaskOutcome{Joined: true}, nil
		}
		if output := endedRunOutput(row); output != nil {
			if output.Status == agentstate.RunStatusError {
				return AgentTaskOutcome{}, fmt.Errorf("the run this message started on thread %s failed", input.ThreadID)
			}
			return AgentTaskOutcome{Output: output}, nil
		}
		// The run an earlier attempt started is still going.
		select {
		case <-ctx.Done():
			return AgentTaskOutcome{}, ctx.Err()
		case <-time.After(agentTaskPollInterval):
		}
	}

	if claimer, ok := agent.StreamBroker().(RunClaimBroker); ok {
		started, err := claimer.EnqueueOrStart(ctx, input.StreamID, []history.Message{input.Message})
		if err != nil {
			return AgentTaskOutcome{}, err
		}
		if !started {
			return AgentTaskOutcome{Joined: true}, nil
		}
	}

	output, err := agent.Run(ctx, input)
	if err != nil {
		return AgentTaskOutcome{}, err
	}
	return AgentTaskOutcome{Output: output}, nil
}

// findAgentTaskMessage finds the stored turn holding the task's message, and
// whether the message is the one that opened it.
func findAgentTaskMessage(ctx context.Context, agent *Agent, input *AgentInput) (*history.ConversationMessage, bool, error) {
	manager := agent.History()
	if manager == nil || manager.ConversationPersistenceAdapter == nil || input.Message.ID == "" {
		return nil, false, nil
	}
	rows, err := history.LoadTranscript(ctx, manager.ConversationPersistenceAdapter, input.Namespace, input.ThreadID)
	if err != nil {
		return nil, false, fmt.Errorf("look for the task's message on thread %s: %w", input.ThreadID, err)
	}
	for i := range rows {
		for position, bundle := range rows[i].Messages {
			if bundle.ID == input.Message.ID {
				return &rows[i], position == 0, nil
			}
		}
	}
	return nil, false, nil
}

// endedRunOutput is a stored run's outcome once it has ended, or nil while it
// is still going.
func endedRunOutput(row *history.ConversationMessage) *AgentOutput {
	state := agentstate.LoadRunStateFromMeta(row.Meta)
	var status agentstate.RunStatus
	switch {
	case state.IsComplete():
		status = agentstate.RunStatusCompleted
	case state.IsPaused():
		status = agentstate.RunStatusPaused
	case state.IsFailed():
		status = agentstate.RunStatusError
	default:
		return nil
	}

	// The run's output, as AgentOutput.Output carries it: what the agent
	// added to the turn, not the messages it took in.
	output := &AgentOutput{RunID: row.RunID, Status: status}
	for _, bundle := range row.Messages[1:] {
		for _, msg := range bundle.Messages {
			if isAgentOutput(msg) {
				output.Output = append(output.Output, msg)
			}
		}
	}
	return output
}

func isAgentOutput(msg responses.InputMessageUnion) bool {
	switch {
	case msg.OfOutputMessage != nil, msg.OfFunctionCall != nil, msg.OfFunctionCallOutput != nil:
		return true
	case msg.OfEasyInput != nil:
		// An assistant reply that crossed a durable step as JSON.
		return msg.OfEasyInput.Role == constants.RoleAssistant
	}
	return false
}
