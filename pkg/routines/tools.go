package routines

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
	"github.com/invopop/jsonschema"
)

// RoutineTools constructs individual tools bound to a service and optional
// scheduler. Each method returns a new, independently configurable tool.
type RoutineTools struct {
	service   *Service
	scheduler Scheduler
}

// Tools returns a factory so callers can select only the operations an agent
// needs. Tools use ToolCall.Namespace, never a namespace supplied by the model.
// The first optional scheduler is used by GetRoutineStatusTool.
func Tools(service *Service, schedulers ...Scheduler) *RoutineTools {
	factory := &RoutineTools{service: service}
	if len(schedulers) > 0 {
		factory.scheduler = schedulers[0]
	}
	return factory
}

type createRoutineArgs struct {
	Routine *Definition `json:"routine"`
}

type createRoutineTool struct {
	*agents.BaseTool
	service *Service
}

// CreateRoutineTool creates enabled routine definitions.
func (t *RoutineTools) CreateRoutineTool() agents.Tool {
	return &createRoutineTool{
		BaseTool: &agents.BaseTool{ToolUnion: responses.ToolUnion{OfFunction: &responses.FunctionTool{
			Name:        "routines_create",
			Description: utils.Ptr("Create a scheduled agent routine. Choose exactly one future datetime or five-field cron schedule."),
			Parameters:  routineToolSchema(createRoutineArgs{}),
		}}},
		service: t.service,
	}
}

func (t *createRoutineTool) Execute(ctx context.Context, call *agents.ToolCall) (*agents.ToolCallResponse, error) {
	if call == nil || call.FunctionCallMessage == nil {
		return nil, fmt.Errorf("missing tool call")
	}
	var args *createRoutineArgs
	dec := json.NewDecoder(strings.NewReader(call.Arguments))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&args); err != nil {
		return nil, err
	}
	if args == nil {
		return nil, fmt.Errorf("expected one JSON object")
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("expected one JSON object")
	}
	if args.Routine == nil {
		return nil, fmt.Errorf("routine is required")
	}

	value, err := t.service.Create(ctx, call.Namespace, *args.Routine)
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return &agents.ToolCallResponse{FunctionCallOutputMessage: &responses.FunctionCallOutputMessage{
		ID: call.ID, CallID: call.CallID,
		Output: responses.FunctionCallOutputContentUnion{OfString: utils.Ptr(string(data))},
	}}, nil
}

type listRoutinesArgs struct {
}

type listRoutinesTool struct {
	*agents.BaseTool
	service *Service
}

// ListRoutinesTool lists definitions in the calling namespace.
func (t *RoutineTools) ListRoutinesTool() agents.Tool {
	return &listRoutinesTool{
		BaseTool: &agents.BaseTool{ToolUnion: responses.ToolUnion{OfFunction: &responses.FunctionTool{
			Name:        "routines_list",
			Description: utils.Ptr("List routine definitions in the current namespace."),
			Parameters:  routineToolSchema(listRoutinesArgs{}),
		}}},
		service: t.service,
	}
}

func (t *listRoutinesTool) Execute(ctx context.Context, call *agents.ToolCall) (*agents.ToolCallResponse, error) {
	if call == nil || call.FunctionCallMessage == nil {
		return nil, fmt.Errorf("missing tool call")
	}
	var args *listRoutinesArgs
	dec := json.NewDecoder(strings.NewReader(call.Arguments))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&args); err != nil {
		return nil, err
	}
	if args == nil {
		return nil, fmt.Errorf("expected one JSON object")
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("expected one JSON object")
	}

	value, err := t.service.List(ctx, call.Namespace)
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return &agents.ToolCallResponse{FunctionCallOutputMessage: &responses.FunctionCallOutputMessage{
		ID: call.ID, CallID: call.CallID,
		Output: responses.FunctionCallOutputContentUnion{OfString: utils.Ptr(string(data))},
	}}, nil
}

type getRoutineArgs struct {
	ID string `json:"id" jsonschema_description:"Routine ID"`
}

type getRoutineTool struct {
	*agents.BaseTool
	service *Service
}

// GetRoutineTool reads one routine definition.
func (t *RoutineTools) GetRoutineTool() agents.Tool {
	return &getRoutineTool{
		BaseTool: &agents.BaseTool{ToolUnion: responses.ToolUnion{OfFunction: &responses.FunctionTool{
			Name:        "routines_get",
			Description: utils.Ptr("Get a routine by ID in the current namespace."),
			Parameters:  routineToolSchema(getRoutineArgs{}),
		}}},
		service: t.service,
	}
}

func (t *getRoutineTool) Execute(ctx context.Context, call *agents.ToolCall) (*agents.ToolCallResponse, error) {
	if call == nil || call.FunctionCallMessage == nil {
		return nil, fmt.Errorf("missing tool call")
	}
	var args *getRoutineArgs
	dec := json.NewDecoder(strings.NewReader(call.Arguments))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&args); err != nil {
		return nil, err
	}
	if args == nil {
		return nil, fmt.Errorf("expected one JSON object")
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("expected one JSON object")
	}
	if strings.TrimSpace(args.ID) == "" {
		return nil, fmt.Errorf("id is required")
	}

	value, err := t.service.Get(ctx, call.Namespace, args.ID)
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return &agents.ToolCallResponse{FunctionCallOutputMessage: &responses.FunctionCallOutputMessage{
		ID: call.ID, CallID: call.CallID,
		Output: responses.FunctionCallOutputContentUnion{OfString: utils.Ptr(string(data))},
	}}, nil
}

type updateRoutineArgs struct {
	ID      string      `json:"id" jsonschema_description:"Routine ID"`
	Routine *Definition `json:"routine"`
}

type updateRoutineTool struct {
	*agents.BaseTool
	service *Service
}

// UpdateRoutineTool replaces a routine definition.
func (t *RoutineTools) UpdateRoutineTool() agents.Tool {
	return &updateRoutineTool{
		BaseTool: &agents.BaseTool{ToolUnion: responses.ToolUnion{OfFunction: &responses.FunctionTool{
			Name:        "routines_update",
			Description: utils.Ptr("Replace a routine's name, target agent, instruction and schedule."),
			Parameters:  routineToolSchema(updateRoutineArgs{}),
		}}},
		service: t.service,
	}
}

func (t *updateRoutineTool) Execute(ctx context.Context, call *agents.ToolCall) (*agents.ToolCallResponse, error) {
	if call == nil || call.FunctionCallMessage == nil {
		return nil, fmt.Errorf("missing tool call")
	}
	var args *updateRoutineArgs
	dec := json.NewDecoder(strings.NewReader(call.Arguments))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&args); err != nil {
		return nil, err
	}
	if args == nil {
		return nil, fmt.Errorf("expected one JSON object")
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("expected one JSON object")
	}
	if strings.TrimSpace(args.ID) == "" {
		return nil, fmt.Errorf("id is required")
	}
	if args.Routine == nil {
		return nil, fmt.Errorf("routine is required")
	}

	value, err := t.service.Update(ctx, call.Namespace, args.ID, *args.Routine)
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return &agents.ToolCallResponse{FunctionCallOutputMessage: &responses.FunctionCallOutputMessage{
		ID: call.ID, CallID: call.CallID,
		Output: responses.FunctionCallOutputContentUnion{OfString: utils.Ptr(string(data))},
	}}, nil
}

type deleteRoutineArgs struct {
	ID string `json:"id" jsonschema_description:"Routine ID"`
}

type deleteRoutineTool struct {
	*agents.BaseTool
	service *Service
}

// DeleteRoutineTool deletes a routine definition.
func (t *RoutineTools) DeleteRoutineTool() agents.Tool {
	return &deleteRoutineTool{
		BaseTool: &agents.BaseTool{ToolUnion: responses.ToolUnion{OfFunction: &responses.FunctionTool{
			Name:        "routines_delete",
			Description: utils.Ptr("Delete a routine."),
			Parameters:  routineToolSchema(deleteRoutineArgs{}),
		}}},
		service: t.service,
	}
}

func (t *deleteRoutineTool) Execute(ctx context.Context, call *agents.ToolCall) (*agents.ToolCallResponse, error) {
	if call == nil || call.FunctionCallMessage == nil {
		return nil, fmt.Errorf("missing tool call")
	}
	var args *deleteRoutineArgs
	dec := json.NewDecoder(strings.NewReader(call.Arguments))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&args); err != nil {
		return nil, err
	}
	if args == nil {
		return nil, fmt.Errorf("expected one JSON object")
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("expected one JSON object")
	}
	if strings.TrimSpace(args.ID) == "" {
		return nil, fmt.Errorf("id is required")
	}

	if err := t.service.Delete(ctx, call.Namespace, args.ID); err != nil {
		return nil, err
	}
	value := map[string]bool{"deleted": true}
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return &agents.ToolCallResponse{FunctionCallOutputMessage: &responses.FunctionCallOutputMessage{
		ID: call.ID, CallID: call.CallID,
		Output: responses.FunctionCallOutputContentUnion{OfString: utils.Ptr(string(data))},
	}}, nil
}

type pauseRoutineArgs struct {
	ID string `json:"id" jsonschema_description:"Routine ID"`
}

type pauseRoutineTool struct {
	*agents.BaseTool
	service *Service
}

// PauseRoutineTool disables a routine.
func (t *RoutineTools) PauseRoutineTool() agents.Tool {
	return &pauseRoutineTool{
		BaseTool: &agents.BaseTool{ToolUnion: responses.ToolUnion{OfFunction: &responses.FunctionTool{
			Name:        "routines_pause",
			Description: utils.Ptr("Pause future routine occurrences. Active execution is cancelled during reconciliation."),
			Parameters:  routineToolSchema(pauseRoutineArgs{}),
		}}},
		service: t.service,
	}
}

func (t *pauseRoutineTool) Execute(ctx context.Context, call *agents.ToolCall) (*agents.ToolCallResponse, error) {
	if call == nil || call.FunctionCallMessage == nil {
		return nil, fmt.Errorf("missing tool call")
	}
	var args *pauseRoutineArgs
	dec := json.NewDecoder(strings.NewReader(call.Arguments))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&args); err != nil {
		return nil, err
	}
	if args == nil {
		return nil, fmt.Errorf("expected one JSON object")
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("expected one JSON object")
	}
	if strings.TrimSpace(args.ID) == "" {
		return nil, fmt.Errorf("id is required")
	}

	value, err := t.service.SetEnabled(ctx, call.Namespace, args.ID, false)
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return &agents.ToolCallResponse{FunctionCallOutputMessage: &responses.FunctionCallOutputMessage{
		ID: call.ID, CallID: call.CallID,
		Output: responses.FunctionCallOutputContentUnion{OfString: utils.Ptr(string(data))},
	}}, nil
}

type resumeRoutineArgs struct {
	ID string `json:"id" jsonschema_description:"Routine ID"`
}

type resumeRoutineTool struct {
	*agents.BaseTool
	service *Service
}

// ResumeRoutineTool enables a routine with a new definition revision.
func (t *RoutineTools) ResumeRoutineTool() agents.Tool {
	return &resumeRoutineTool{
		BaseTool: &agents.BaseTool{ToolUnion: responses.ToolUnion{OfFunction: &responses.FunctionTool{
			Name:        "routines_resume",
			Description: utils.Ptr("Enable a paused routine. One-time schedules must still be in the future."),
			Parameters:  routineToolSchema(resumeRoutineArgs{}),
		}}},
		service: t.service,
	}
}

func (t *resumeRoutineTool) Execute(ctx context.Context, call *agents.ToolCall) (*agents.ToolCallResponse, error) {
	if call == nil || call.FunctionCallMessage == nil {
		return nil, fmt.Errorf("missing tool call")
	}
	var args *resumeRoutineArgs
	dec := json.NewDecoder(strings.NewReader(call.Arguments))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&args); err != nil {
		return nil, err
	}
	if args == nil {
		return nil, fmt.Errorf("expected one JSON object")
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("expected one JSON object")
	}
	if strings.TrimSpace(args.ID) == "" {
		return nil, fmt.Errorf("id is required")
	}

	value, err := t.service.SetEnabled(ctx, call.Namespace, args.ID, true)
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return &agents.ToolCallResponse{FunctionCallOutputMessage: &responses.FunctionCallOutputMessage{
		ID: call.ID, CallID: call.CallID,
		Output: responses.FunctionCallOutputContentUnion{OfString: utils.Ptr(string(data))},
	}}, nil
}

type listAgentsArgs struct {
}

type listAgentsTool struct {
	*agents.BaseTool
	service *Service
}

// ListAgentsTool lists available target agents.
func (t *RoutineTools) ListAgentsTool() agents.Tool {
	return &listAgentsTool{
		BaseTool: &agents.BaseTool{ToolUnion: responses.ToolUnion{OfFunction: &responses.FunctionTool{
			Name:        "routines_agents",
			Description: utils.Ptr("List available target agent names."),
			Parameters:  routineToolSchema(listAgentsArgs{}),
		}}},
		service: t.service,
	}
}

func (t *listAgentsTool) Execute(ctx context.Context, call *agents.ToolCall) (*agents.ToolCallResponse, error) {
	if call == nil || call.FunctionCallMessage == nil {
		return nil, fmt.Errorf("missing tool call")
	}
	var args *listAgentsArgs
	dec := json.NewDecoder(strings.NewReader(call.Arguments))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&args); err != nil {
		return nil, err
	}
	if args == nil {
		return nil, fmt.Errorf("expected one JSON object")
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("expected one JSON object")
	}

	value := []string{}
	if registry, ok := t.service.executor.(interface{ AgentNames() []string }); ok {
		value = append(value, registry.AgentNames()...)
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return &agents.ToolCallResponse{FunctionCallOutputMessage: &responses.FunctionCallOutputMessage{
		ID: call.ID, CallID: call.CallID,
		Output: responses.FunctionCallOutputContentUnion{OfString: utils.Ptr(string(data))},
	}}, nil
}

type getRoutineStatusArgs struct {
	ID string `json:"id" jsonschema_description:"Routine ID"`
}

type getRoutineStatusTool struct {
	*agents.BaseTool
	service   *Service
	scheduler Scheduler
}

// GetRoutineStatusTool reads scheduler-owned state. Execution returns an error
// if no scheduler was supplied to Tools.
func (t *RoutineTools) GetRoutineStatusTool() agents.Tool {
	return &getRoutineStatusTool{
		BaseTool: &agents.BaseTool{ToolUnion: responses.ToolUnion{OfFunction: &responses.FunctionTool{
			Name:        "routines_status",
			Description: utils.Ptr("Read execution state from the routine scheduler."),
			Parameters:  routineToolSchema(getRoutineStatusArgs{}),
		}}},
		service: t.service, scheduler: t.scheduler,
	}
}

func (t *getRoutineStatusTool) Execute(ctx context.Context, call *agents.ToolCall) (*agents.ToolCallResponse, error) {
	if call == nil || call.FunctionCallMessage == nil {
		return nil, fmt.Errorf("missing tool call")
	}
	var args *getRoutineStatusArgs
	dec := json.NewDecoder(strings.NewReader(call.Arguments))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&args); err != nil {
		return nil, err
	}
	if args == nil {
		return nil, fmt.Errorf("expected one JSON object")
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("expected one JSON object")
	}
	if strings.TrimSpace(args.ID) == "" {
		return nil, fmt.Errorf("id is required")
	}

	if t.scheduler == nil {
		return nil, fmt.Errorf("routine status requires a scheduler supplied to Tools")
	}
	if _, err := t.service.Get(ctx, call.Namespace, args.ID); err != nil {
		return nil, err
	}
	value, err := t.scheduler.Status(ctx, call.Namespace, args.ID)
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return &agents.ToolCallResponse{FunctionCallOutputMessage: &responses.FunctionCallOutputMessage{
		ID: call.ID, CallID: call.CallID,
		Output: responses.FunctionCallOutputContentUnion{OfString: utils.Ptr(string(data))},
	}}, nil
}

// routineToolSchema reflects the same argument types decoded by Execute.
func routineToolSchema(args any) map[string]any {
	reflector := jsonschema.Reflector{ExpandedStruct: true, DoNotReference: true}
	schema := reflector.Reflect(args)
	schema.Version = ""
	schema.ID = ""
	data, err := schema.MarshalJSON()
	if err != nil {
		panic(fmt.Errorf("marshal routine tool schema: %w", err))
	}
	var parameters map[string]any
	if err := json.Unmarshal(data, &parameters); err != nil {
		panic(fmt.Errorf("decode routine tool schema: %w", err))
	}
	return parameters
}
