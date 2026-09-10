package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/agents/agentstate"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/hastekit/agent-sdk-go/pkg/agents/messages"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
)

// BackgroundTaskRef identifies one background task to the tool waiting on it,
// and tells that tool where its progress and result belong.
//
// Every field is a plain value, so the same struct describes a task whether
// the wait happens in this process or somewhere that had to be told about it.
type BackgroundTaskRef struct {
	// TaskID is what the tool called the task when it answered. It is the
	// tool's own identifier — a job id from whatever service it queued work
	// with — and nothing here does more than carry it back.
	TaskID string `json:"task_id"`

	// CallID is the tool call that started the task. Progress published while
	// the task runs is keyed to it, so a client that already drew the call
	// updates that row rather than growing a new one.
	CallID string `json:"call_id"`

	ToolName string `json:"tool_name"`

	// AgentName is the agent the run entered at — the one that owns the
	// thread, and the one a run started by this task's result enters at.
	// After a handoff that is not the agent whose loop called the tool.
	AgentName string `json:"agent_name"`

	Namespace string `json:"namespace"`
	ThreadID  string `json:"thread_id"`

	// TaskStreamID is the task's own broker channel, where its progress is
	// published. Nothing else writes to it and no run resets it, so progress
	// survives whatever the thread does in the meantime and stays replayable
	// to a client that subscribes late. It closes when the task ends.
	TaskStreamID string `json:"task_stream_id"`

	// ThreadStreamID is the thread's channel, where the result is delivered
	// when the task ends — into the run holding it, or as a new run.
	ThreadStreamID string `json:"thread_stream_id"`

	// RunID is the run that started the task, for correlation. The run itself
	// is usually finished by the time the task is.
	RunID string `json:"run_id,omitempty"`

	// RunContext is the run context of the call that started the task, so a
	// run started by its completion carries the same per-tenant data.
	RunContext map[string]any `json:"run_context,omitempty"`

	// Payload is whatever the tool set on the response that started the task.
	// It is how the call's arguments reach the wait, which happens in another
	// call and, under a durable runtime, in another process.
	Payload json.RawMessage `json:"payload,omitempty"`
}

// BackgroundResult is what a finished task has to tell the model.
type BackgroundResult struct {
	// Output is the result, in the same shape a tool returns from Execute —
	// so a task can answer with an image or a file, not only a line of text.
	//
	// Its CallID is not used: the call that started the task was answered when
	// it returned, and a provider will not take a second output against it.
	// Only the content travels.
	Output *responses.FunctionCallOutputMessage `json:"output,omitempty"`
}

// BackgroundText is the ordinary result: a line of text for the model.
func BackgroundText(text string) *responses.FunctionCallOutputMessage {
	return &responses.FunctionCallOutputMessage{
		Output: responses.FunctionCallOutputContentUnion{OfString: &text},
	}
}

// BackgroundTool is an optional Tool capability: the tool starts work that
// outlives the call, answers immediately with a task id, and is asked later to
// wait for the outcome.
//
// Execute returns a ToolCallResponse carrying TaskID — its Output is what the
// model reads now ("started indexing, job 41ff"), and the run carries on. The
// agent then calls AwaitTask, and whatever that returns is delivered to the
// thread when it returns: into the run that started the task if it is still
// going, or as a new run if the agent has since gone idle.
//
// AwaitTask must block until the task is done. Poll, subscribe, wait on a
// channel — whatever the underlying service offers. Report progress through
// the reporter as it goes; it publishes the same tool.progress chunks the tool
// itself can emit during Execute, so a client sees one continuous account of
// the call whether the work happened inside it or after it.
//
// Progress publishes to the task's own channel, named by
// BackgroundTaskRef.TaskStreamID, not to the thread's. It therefore survives
// whatever the thread does while the task runs, and a client that subscribes
// late still gets the whole account of it. The channel closes when the task
// ends.
//
// Returning an error is not the same as a task that failed. An error means the
// wait itself broke down — the service became unreachable, the poll gave up —
// and is reported to the model as such. A task that ran and failed is an
// ordinary result whose Output says so.
type BackgroundTool interface {
	Tool

	AwaitTask(ctx context.Context, task BackgroundTaskRef, progress ProgressReporter) (BackgroundResult, error)
}

// BackgroundRunner begins the wait for a background task. It is how a runtime
// says what "outliving the call that started it" means where it runs.
//
// In this process it is a goroutine. Under a durable runtime the wait has to
// outlive the activity or step the tool ran in, so it becomes something that
// runtime can keep: a detached child workflow under Temporal, a one-way call
// to another handler under Restate.
//
// StartTask must return promptly. The run does not wait for the task, and a
// runner that blocked here would make it.
type BackgroundRunner interface {
	StartTask(ctx context.Context, tool BackgroundTool, ref BackgroundTaskRef) error
}

// DeliverBackgroundResult puts a finished task's outcome in front of the
// agent, and reports whether a new run has to be started to react to it.
//
// The decision is shared by every runtime because getting it wrong is the same
// mistake everywhere: joining a run that has ended strands the result, and
// starting one that has not leaves two runs writing one stream. Only the act
// of starting differs — a goroutine here, a workflow there — and that is left
// to the caller.
//
// A true return means the caller now holds the channel's claim and must start
// a run with msg as its input.
func DeliverBackgroundResult(
	ctx context.Context,
	broker StreamBroker,
	ref BackgroundTaskRef,
	result BackgroundResult,
	awaitErr error,
) (start bool, msg history.Message, err error) {
	msg = messages.New("", []responses.InputMessageUnion{backgroundNotice(ref, result, awaitErr)})
	// Bookkeeping, not announcement: this is how the run that picks the result
	// up knows which of the tasks it is carrying has just landed.
	msg.BackgroundTaskID = ref.TaskID

	if broker == nil || ref.ThreadStreamID == "" {
		return false, msg, fmt.Errorf("background task %s finished with nowhere to report", ref.TaskID)
	}

	// Is a run still going on this thread? IsActive is the question to ask,
	// not the claim: a run started straight through Execute never took the
	// claim, so asking the claim would answer "idle" and start a second run
	// on a channel that already has one. IsActive counts the subscription the
	// running loop holds, so it sees both kinds.
	active, err := broker.IsActive(ctx, ref.ThreadStreamID)
	if err != nil {
		return false, msg, fmt.Errorf("checking whether a run is live for background task %s: %w", ref.TaskID, err)
	}

	if active {
		// The run drains the queue at its next iteration boundary, the same
		// cadence as a steering message.
		//
		// A run that ends in the moment between that check and this enqueue
		// leaves the result on the queue for the thread's next turn instead of
		// waking the agent now. Nothing is lost, and the alternative — claiming
		// the channel on a guess — risks two runs writing one stream.
		if err := broker.EnqueueMessage(ctx, ref.ThreadStreamID, msg); err != nil {
			return false, msg, fmt.Errorf("delivering background task %s: %w", ref.TaskID, err)
		}
		publishTaskCompleted(ctx, broker, ref)
		return false, msg, nil
	}

	claimer, ok := broker.(RunClaimBroker)
	if !ok {
		// Without the atomic claim there is no safe way to start a run, so the
		// result waits on the queue for whatever runs next.
		if err := broker.EnqueueMessage(ctx, ref.ThreadStreamID, msg); err != nil {
			return false, msg, fmt.Errorf("delivering background task %s: %w", ref.TaskID, err)
		}
		return false, msg, nil
	}

	started, err := claimer.EnqueueOrStart(ctx, ref.ThreadStreamID, []history.Message{msg})
	if err != nil {
		return false, msg, fmt.Errorf("delivering background task %s: %w", ref.TaskID, err)
	}

	// Claiming resets the channel and holds it, so this lands on the run that
	// is about to start rather than on the remains of the one that ended. It
	// arrives before that run's own opening chunk, which is why a reader has
	// to hold back what precedes a run rather than discard it.
	//
	// started=false means a turn claimed the channel in between. It owns the
	// queue this message is now on and will drain it — and it is streaming, so
	// the announcement belongs on it either way.
	publishTaskCompleted(ctx, broker, ref)

	return started, msg, nil
}

// publishTaskCompleted announces on the thread's stream that a task's result
// has arrived.
//
// Here rather than in the agent loop because this is where it is known: the
// loop is handed an ordinary user turn — the call that started the task was
// answered when the tool returned, and a provider will not take a second
// output against it — so it would have to be told, whereas this has the task,
// the call and the tool already in hand.
//
// Best effort. A result is not worth failing over an announcement, and the
// result itself is on the queue regardless.
func publishTaskCompleted(ctx context.Context, broker StreamBroker, ref BackgroundTaskRef) {
	if broker == nil || ref.ThreadStreamID == "" {
		return
	}

	err := broker.Publish(ctx, ref.ThreadStreamID, &responses.ResponseChunk{
		OfBackgroundTaskCompleted: &responses.ChunkBackgroundTask[constants.ChunkTypeBackgroundTaskCompleted]{
			TaskID:   ref.TaskID,
			CallID:   ref.CallID,
			ToolName: ref.ToolName,
			StreamID: ref.TaskStreamID,
		},
	})
	if err != nil {
		slog.WarnContext(ctx, "failed to announce a background task result",
			slog.String("task_id", ref.TaskID), slog.Any("error", err))
	}
}

// CloseBackgroundTaskStream ends a task's own stream, so a client watching it
// sees the task end when it ends rather than when the agent has finished
// reacting to it.
func CloseBackgroundTaskStream(ctx context.Context, broker StreamBroker, ref BackgroundTaskRef) error {
	if broker == nil || ref.TaskStreamID == "" {
		return nil
	}
	return broker.Close(ctx, ref.TaskStreamID)
}

// ErrBackgroundUnsupported is returned when a tool starts a background task
// under a runtime that cannot wait for one.
//
// The wait outlives the call that started it, which is exactly what a durable
// runtime cannot express as an ordinary goroutine: the activity or step ends
// and takes the wait with it, so the task would run to completion and no one
// would ever hear. Failing the run says so, rather than leaving a task that
// silently never lands.
var ErrBackgroundUnsupported = fmt.Errorf("background tool tasks are not supported by this runtime")

// backgroundSupervisor waits on the tasks an agent's tools have started, and
// delivers each result back to the thread it belongs to.
//
// It holds the waits in memory, so they last as long as the process does. A
// restart loses them; what the run recorded in RunState.BackgroundTasks is the
// account of what was started, for whatever comes to reconcile it.
// The agent it runs is rebound by WithLLM: an agent copied wholesale would
// otherwise keep a supervisor pointing at the original, and a task delivered
// through it would run that agent's model rather than the caller's.
type backgroundSupervisor struct {
	agent *Agent

	mu      sync.Mutex
	running map[string]struct{}

	// wg tracks the waits in flight. Tests use it to wait for delivery
	// instead of sleeping.
	wg sync.WaitGroup
}

func newBackgroundSupervisor(agent *Agent) *backgroundSupervisor {
	return &backgroundSupervisor{agent: agent, running: map[string]struct{}{}}
}

var _ BackgroundRunner = (*backgroundSupervisor)(nil)

// StartTask holds the wait in a goroutine. The tool already carries its
// execution middleware; the supervisor only owns task lifecycle and delivery.
func (s *backgroundSupervisor) StartTask(_ context.Context, tool BackgroundTool, ref BackgroundTaskRef) error {
	if s == nil || s.agent == nil {
		return nil
	}
	s.startTask(tool, ref)
	return nil
}

// startTask begins waiting on a task, unless it is already being waited on — a
// tool that answers with the same task id twice gets one wait, not two.
func (s *backgroundSupervisor) startTask(tool BackgroundTool, ref BackgroundTaskRef) {
	agent := s.agent
	if tool == nil || ref.TaskID == "" {
		return
	}

	s.mu.Lock()
	if _, already := s.running[ref.TaskID]; already {
		s.mu.Unlock()
		return
	}
	s.running[ref.TaskID] = struct{}{}
	s.mu.Unlock()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() {
			s.mu.Lock()
			delete(s.running, ref.TaskID)
			s.mu.Unlock()
		}()

		// Not the run's context: the run that started this task is expected to
		// end long before the task does, and its cancellation says nothing
		// about whether the work is still worth waiting for.
		ctx := context.Background()

		progress := NewStreamProgressReporter(agent.streamBroker, ref.TaskStreamID, ref.CallID, ref.ToolName)
		result, err := tool.AwaitTask(ctx, ref, progress)

		// Close before delivering: a subscriber watching the task sees it end
		// when it ends, rather than when the agent has finished reacting to it.
		if err := CloseBackgroundTaskStream(ctx, agent.streamBroker, ref); err != nil {
			slog.ErrorContext(ctx, "failed to close a background task stream",
				slog.String("task_id", ref.TaskID), slog.Any("error", err))
		}

		s.deliver(ctx, agent, ref, result, err)
	}()
}

// wait blocks until every task in flight has been delivered. It is for tests
// and for shutdown; nothing in the loop waits on a background task.
func (s *backgroundSupervisor) wait() {
	if s != nil {
		s.wg.Wait()
	}
}

// deliver puts the outcome in front of the agent, starting a run for it when
// the thread has gone idle.
func (s *backgroundSupervisor) deliver(ctx context.Context, agent *Agent, ref BackgroundTaskRef, result BackgroundResult, awaitErr error) {
	start, msg, err := DeliverBackgroundResult(ctx, agent.streamBroker, ref, result, awaitErr)
	if err != nil {
		slog.ErrorContext(ctx, "failed to deliver background task result",
			slog.String("task_id", ref.TaskID), slog.Any("error", err))
		return
	}
	if !start {
		return
	}

	handle, err := agent.Execute(ctx, &AgentInput{
		Namespace:  ref.Namespace,
		ThreadID:   ref.ThreadID,
		StreamID:   ref.ThreadStreamID,
		RunContext: ref.RunContext,
		Message:    msg,
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed to start a run for a background task result",
			slog.String("task_id", ref.TaskID), slog.Any("error", err))
		return
	}

	// Drain. Nobody asked for this run, so nobody is reading its stream, and a
	// subscription left unread eventually blocks the run publishing into it.
	if _, err := handle.Result(); err != nil {
		slog.ErrorContext(ctx, "run started by a background task result failed",
			slog.String("task_id", ref.TaskID), slog.Any("error", err))
	}
}

// backgroundNotice is the message a finished task puts in front of the model.
//
// It is framed rather than dressed up as a tool result: the call that started
// the task was answered when it returned, and a second output against the same
// call id is not something a provider will accept. So the outcome arrives as
// its own turn, saying plainly where it came from — and carrying the result's
// own content blocks after it, so an image or a file survives the trip.
func backgroundNotice(ref BackgroundTaskRef, result BackgroundResult, awaitErr error) responses.InputMessageUnion {
	var text string
	if awaitErr != nil {
		text = fmt.Sprintf(
			"[Background task %s, started by the %s tool, could not be completed: %v. "+
				"Tell the user what happened and decide whether to try again.]",
			ref.TaskID, ref.ToolName, awaitErr)
	} else {
		text = fmt.Sprintf(
			"[Background task %s, started by the %s tool, has finished. Its result follows; "+
				"account for it before deciding what to do next.]",
			ref.TaskID, ref.ToolName)
	}

	content := responses.InputContent{
		{OfInputText: &responses.InputTextContent{Text: text}},
	}
	if awaitErr == nil {
		content = append(content, backgroundOutputContent(result.Output)...)
	}

	return responses.InputMessageUnion{
		OfInputMessage: &responses.InputMessage{Role: constants.RoleUser, Content: content},
	}
}

// backgroundOutputContent turns a task's output into the content blocks that
// follow the notice. A string becomes one text block; a list travels as it is,
// which is what carries an image or a file through.
func backgroundOutputContent(out *responses.FunctionCallOutputMessage) responses.InputContent {
	if out == nil {
		return nil
	}
	switch {
	case out.Output.OfString != nil:
		return responses.InputContent{
			{OfInputText: &responses.InputTextContent{Text: *out.Output.OfString}},
		}
	case out.Output.OfList != nil:
		return out.Output.OfList
	}
	return nil
}

// startBackgroundTask records a task the tool just started and begins waiting
// on it.
//
// It fails the run rather than carrying on when the task cannot be waited on —
// because the tool has, by then, already started the work. A run that shrugged
// and continued would leave the model believing a result was coming, the user
// waiting for it, and the task itself running somewhere with nothing listening.
func (e *Agent) startBackgroundTask(ctx context.Context, in *AgentInput, runID string, pe ExecutableToolCall, resp *ToolCallResponse, state *agentstate.RunState) error {
	taskID := resp.TaskID

	tool, ok := pe.Tool.(BackgroundTool)
	if !ok {
		return fmt.Errorf("tool %q answered with task id %q but does not implement BackgroundTool, so nothing can wait for it", pe.ToolName, taskID)
	}

	// The task belongs to the run's owner, not to whichever agent in the
	// handoff graph happened to start it. A specialist reached by handoff runs
	// inside someone else's run: its own history is not this conversation, its
	// own broker is not this stream, and entering a later run at it would skip
	// the routing that put it there. Settle all three against the owner.
	owner := in.owner
	if owner == nil {
		owner = e
	}

	if owner.background == nil {
		return fmt.Errorf("%w: tool %q started task %q", ErrBackgroundUnsupported, pe.ToolName, taskID)
	}

	ref := BackgroundTaskRef{
		TaskID:         taskID,
		CallID:         pe.ToolCall.CallID,
		ToolName:       pe.ToolName,
		AgentName:      owner.Name,
		Namespace:      in.Namespace,
		ThreadID:       in.ThreadID,
		TaskStreamID:   StreamIDForTask(in.Namespace, in.ThreadID, taskID),
		ThreadStreamID: in.StreamID,
		RunID:          runID,
		RunContext:     in.RunContext,
		Payload:        resp.TaskPayload,
	}

	state.AddBackgroundTask(agentstate.BackgroundTask{
		TaskID:    taskID,
		CallID:    ref.CallID,
		ToolName:  ref.ToolName,
		StreamID:  ref.TaskStreamID,
		StartedAt: time.Now().UTC(),
	})

	// Bind while the producing agent and tool are already in hand, including
	// tools discovered through MCP. Durable agents have no workflow-side middlewares:
	// their real tools are bound on the worker, and their proxies pass through.
	if len(e.options.Middlewares) > 0 {
		tool = WrapBackgroundTool(e.Name, tool, ToolCallMiddlewaresOf(e.options.Middlewares)...)
	}
	if err := owner.background.StartTask(ctx, tool, ref); err != nil {
		return err
	}

	// Tell whoever is watching this run that the call it just saw answered is
	// still working, and where to follow it.
	publish := e.publisher(in.StreamID)
	e.durableStep.Do(func() {
		publish(&responses.ResponseChunk{
			OfBackgroundTaskStarted: &responses.ChunkBackgroundTask[constants.ChunkTypeBackgroundTaskStarted]{
				TaskID:   ref.TaskID,
				CallID:   ref.CallID,
				ToolName: ref.ToolName,
				StreamID: ref.TaskStreamID,
			},
		})
	})

	return nil
}

// WaitForBackgroundTasks blocks until every background task this agent started
// has been delivered.
//
// No run waits on this: a run that starts a task finishes without it, which is
// the point. It is for shutting down without abandoning work in flight, and
// for tests that need to observe the delivery rather than race it.
//
// A durable runtime keeps its waits outside this process, so there is nothing
// here to wait on and this returns at once.
func (e *Agent) WaitForBackgroundTasks() {
	if w, ok := e.background.(interface{ wait() }); ok {
		w.wait()
	}
}
