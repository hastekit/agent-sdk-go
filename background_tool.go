package sdk

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"runtime"
	"strings"

	sonic "github.com/bytedance/sonic"
	"github.com/google/uuid"
	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
)

// BackgroundTaskRef identifies a running background task to the tool that
// started it.
type BackgroundTaskRef = agents.BackgroundTaskRef

// ProgressReporter is how a running tool says how far along it is.
type ProgressReporter = agents.ProgressReporter

// ToolProgress is one progress update. Total is optional; 0 means unknown.
type ToolProgress = agents.ToolProgress

// BackgroundToolFunc is the work a background tool does. It is handed the
// call's arguments and somewhere to report progress, and runs for as long as it
// needs to — the run that called the tool is long gone by the time it returns.
type BackgroundToolFunc[T any, S any] func(ctx context.Context, in T, progress ProgressReporter) (S, error)

// BackgroundFunctionTool is NewTool for work that does not finish inside the
// call. See NewBackgroundTool.
type BackgroundFunctionTool[T any, S any] struct {
	*FunctionTool[T, S]

	work    BackgroundToolFunc[T, S]
	started func(taskID string) string
}

var _ agents.BackgroundTool = (*BackgroundFunctionTool[any, any])(nil)

// NewBackgroundTool turns a long-running func into a tool that answers at once
// and reports its result when it is done.
//
//	indexTool := hastekit.NewBackgroundTool(
//	    func(ctx context.Context, in IndexArgs, progress hastekit.ProgressReporter) (IndexResult, error) {
//	        for i, doc := range in.Docs {
//	            progress.Report(ctx, hastekit.ToolProgress{
//	                Progress: float64(i), Total: float64(len(in.Docs)), Message: doc.Name,
//	            })
//	            index(doc)
//	        }
//	        return IndexResult{Indexed: len(in.Docs)}, nil
//	    },
//	    hastekit.WithName("index_docs"),
//	    hastekit.WithDescription("Index documents. Takes a while."),
//	)
//
// The model gets an immediate answer naming the task, the run carries on, and
// whatever the func returns is delivered to the thread when it returns — into
// the run that started it if that is still going, otherwise as a new run.
//
// The work runs off the run's path, not inside it: in this process that means
// a goroutine of the agent's, and under Temporal or Restate the activity or
// invocation that runtime keeps for the task. That is why the func is given
// the arguments rather than closing over them — it may well run somewhere the
// call that started it never reached. Anything else it needs must travel the
// same way, in the arguments.
//
// Progress is nil-safe: report freely, whether or not anyone is listening.
//
// The return value is encoded like an ordinary tool's, unless it already is a
// tool output — a *responses.FunctionCallOutputMessage, a
// FunctionCallOutputContentUnion, or a responses.InputContent — in which case
// it travels as it is. That is how a task answers with an image or a file.
//
// For work that has to be *started* now and only watched later — a job queued
// with another service, say — implement agents.BackgroundTool directly, which
// splits starting from waiting.
func NewBackgroundTool[T any, S any](work BackgroundToolFunc[T, S], opts ...ToolOption) *BackgroundFunctionTool[T, S] {
	fnVal := reflect.ValueOf(work)

	bt := &BackgroundFunctionTool[T, S]{
		FunctionTool: &FunctionTool[T, S]{
			name: strings.SplitN(runtime.FuncForPC(fnVal.Pointer()).Name(), ".", 2)[1],
		},
		work:    work,
		started: defaultStartedMessage,
	}

	for _, opt := range opts {
		opt(bt)
	}

	return bt
}

// WithStartedMessage replaces what the model is told the moment the task
// starts. It is given the task id.
//
// The default says the work has started and that its result will follow, which
// is the thing the model most needs to know: that there is nothing to wait for
// and it should carry on.
//
// It applies only to a background tool; on any other tool it does nothing.
func WithStartedMessage(fn func(taskID string) string) ToolOption {
	return func(cfg ToolConfig) {
		if bg, ok := cfg.(backgroundToolConfig); ok && fn != nil {
			bg.SetStartedMessage(fn)
		}
	}
}

// backgroundToolConfig is the one knob a background tool has beyond an
// ordinary one.
type backgroundToolConfig interface {
	SetStartedMessage(func(taskID string) string)
}

func (t *BackgroundFunctionTool[T, S]) SetStartedMessage(fn func(taskID string) string) {
	t.started = fn
}

func defaultStartedMessage(taskID string) string {
	return fmt.Sprintf(
		"Started background task %s. It is running now; its result will arrive on its own when it is done, "+
			"so carry on rather than waiting for it.", taskID)
}

// Execute starts the task: it answers immediately with a task id, and keeps
// the call's arguments for the wait.
func (t *BackgroundFunctionTool[T, S]) Execute(_ context.Context, params *agents.ToolCall) (*agents.ToolCallResponse, error) {
	// Decode now, while there is still a tool call to fail. Arguments that
	// cannot be read are the model's mistake and it can be told; discovering
	// that inside the wait would mean failing a task the model believes is
	// already running.
	var in T
	if err := sonic.Unmarshal([]byte(params.Arguments), &in); err != nil {
		return nil, err
	}

	taskID := uuid.NewString()

	return &agents.ToolCallResponse{
		FunctionCallOutputMessage: &responses.FunctionCallOutputMessage{
			ID:     params.ID,
			CallID: params.CallID,
			Output: responses.FunctionCallOutputContentUnion{OfString: utils.Ptr(t.started(taskID))},
		},
		TaskID: taskID,
		// The arguments as they arrived: already valid JSON, and re-encoding
		// them could only lose something the round trip through T dropped.
		TaskPayload: json.RawMessage(params.Arguments),
	}, nil
}

// AwaitTask runs the work. This is where the tool actually does its job, on
// whatever the runtime keeps alive past the call that started it.
func (t *BackgroundFunctionTool[T, S]) AwaitTask(ctx context.Context, task BackgroundTaskRef, progress ProgressReporter) (agents.BackgroundResult, error) {
	var in T
	if len(task.Payload) > 0 {
		if err := sonic.Unmarshal(task.Payload, &in); err != nil {
			return agents.BackgroundResult{}, fmt.Errorf("background task %s: reading the arguments it started with: %w", task.TaskID, err)
		}
	}

	out, err := t.work(ctx, in, nilSafeProgress{progress})
	if err != nil {
		return agents.BackgroundResult{}, err
	}

	message, err := backgroundOutput(out)
	if err != nil {
		return agents.BackgroundResult{}, err
	}

	return agents.BackgroundResult{Output: message}, nil
}

// backgroundOutput turns whatever the work returned into a tool output.
//
// A value that is already one is passed through untouched: that is how a task
// answers with an image or a file rather than a line of JSON. Anything else is
// encoded, the same way an ordinary function tool's return value is.
func backgroundOutput(out any) (*responses.FunctionCallOutputMessage, error) {
	switch v := out.(type) {
	case *responses.FunctionCallOutputMessage:
		return v, nil
	case responses.FunctionCallOutputMessage:
		return &v, nil
	case responses.FunctionCallOutputContentUnion:
		return &responses.FunctionCallOutputMessage{Output: v}, nil
	case responses.InputContent:
		return &responses.FunctionCallOutputMessage{
			Output: responses.FunctionCallOutputContentUnion{OfList: v},
		}, nil
	}

	encoded, err := sonic.Marshal(out)
	if err != nil {
		return nil, err
	}
	return agents.BackgroundText(string(encoded)), nil
}

// nilSafeProgress lets a tool report progress without asking whether anyone is
// listening. There is no reporter when the agent has no broker or no stream,
// and a tool should not have to care.
type nilSafeProgress struct{ inner ProgressReporter }

func (p nilSafeProgress) Report(ctx context.Context, update ToolProgress) {
	if p.inner != nil {
		p.inner.Report(ctx, update)
	}
}
