package agents

import (
	"context"
	"net/http"
	"time"

	"github.com/a2aproject/a2a-go/a2a"
	"github.com/a2aproject/a2a-go/a2asrv"
	"github.com/a2aproject/a2a-go/a2asrv/eventqueue"
	"github.com/hastekit/hastekit-sdk-go/pkg/agents/history"
	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/hastekit-sdk-go/pkg/utils"
)

type A2A struct {
	agent            *Agent
	agentCard        *a2a.AgentCard
	InvokeHandler    http.Handler
	AgentCardHandler http.Handler
}

func (a *Agent) A2A(agentCard *a2a.AgentCard) *A2A {
	a2aExecutor := &A2A{
		agent:     a,
		agentCard: agentCard,
	}

	requestHandler := a2asrv.NewHandler(a2aExecutor)
	invokeHandler := a2asrv.NewJSONRPCHandler(requestHandler)
	agentCardHandler := a2asrv.NewStaticAgentCardHandler(agentCard)

	a2aExecutor.InvokeHandler = invokeHandler
	a2aExecutor.AgentCardHandler = agentCardHandler

	return a2aExecutor
}

func (agent *A2A) Execute(ctx context.Context, reqCtx *a2asrv.RequestContext, q eventqueue.Queue) error {
	userMsg := reqCtx.Message
	existingTask := reqCtx.StoredTask

	// Determine IDs for the task and context
	var taskId a2a.TaskID
	if existingTask != nil && existingTask.ID != "" {
		taskId = existingTask.ID
	} else {
		taskId = a2a.NewTaskID()
	}

	var contextId string
	if userMsg != nil && userMsg.ContextID != "" {
		contextId = userMsg.ContextID
	} else if existingTask != nil && existingTask.ContextID != "" {
		contextId = existingTask.ContextID
	} else {
		contextId = a2a.NewContextID()
	}

	// Publish initial task event if its a new task
	if existingTask == nil {
		initialTask := &a2a.Task{
			ID:        taskId,
			ContextID: contextId,
			History:   []*a2a.Message{userMsg},
			Metadata:  userMsg.Metadata,
			Status: a2a.TaskStatus{
				State:     a2a.TaskStateSubmitted,
				Timestamp: utils.Ptr(time.Now()),
			},
		}
		//a2a.NewSubmittedTask(reqCtx, userMsg)
		q.Write(ctx, initialTask)
	}

	// Publish "working" status update
	workingStatusUpdate := &a2a.TaskStatusUpdateEvent{
		TaskID:    taskId,
		ContextID: contextId,
		Status: a2a.TaskStatus{
			State: a2a.TaskStateWorking,
			Message: &a2a.Message{
				TaskID:    taskId,
				ContextID: contextId,
				ID:        a2a.NewMessageID(),
				Role:      a2a.MessageRoleAgent,
				Parts: a2a.ContentParts{
					a2a.TextPart{Text: "Processing your question"},
				},
				Extensions:     nil,
				Metadata:       nil,
				ReferenceTasks: nil,
			},
			Timestamp: utils.Ptr(time.Now()),
		},
		Metadata: nil,
		Final:    false,
	}
	q.Write(ctx, workingStatusUpdate)

	// Prepare message to invoke to agent. A2A doesn't surface chunks
	// today, so use Result to drain + aggregate in one step.
	handle, err := agent.agent.Execute(ctx, &AgentInput{
		Namespace:     "",
		PreviousRunID: "",
		Message:       history.Message{Messages: []responses.InputMessageUnion{}},
	})
	if err != nil {
		return nil
	}
	_, _ = handle.Result()

	return nil
}

func (*A2A) Cancel(ctx context.Context, reqCtx *a2asrv.RequestContext, q eventqueue.Queue) error {
	return nil
}

/*
3 layers
1. Data Model
	- Task
	- Message
	- Agent Card
	- Part
	- Artifact
	- Extension
2. Operations
	- Send Message
	- Stream Message
	- Get Task
	- List Tasks
	- Cancel Task
	- Get Agent Card
3. Protocol Bindings
	- JSON RPC
	- GRPC RPC
	- Rest Endpoints
	- Custom Bindings
---
Definitions
1. A2A Client - application or agent that initiates the request to an a2a server on behalf of a user or another system
2. A2A Server - agent that exposes an a2a compliant endpoint, processing tasks and provding responses
3. Agent card - json metadata doc published by a2a server, describing its identity, capabilities, skill, endpoints and auth requirements
4. Message - a turn between client and agent, having role "user" or "agent" and containing one or more "parts"
5. Task - fundamental unit of work, identified by unique id. Tasks are stateful and progress through a defined lifecycle
6. Part - smallest unit of content within a message or artifact (text, file, data)
7. Artifact - An output of the agent for a given task.
8. Streaming - incremental updates for task (status updates, and artifact chunks)
9. Push notification - asynchronous task updates delivered via server initiated http post requests to client provider url.
10. Context - an optional, server-generated identifier to logically group related tasks and messages.
11. Extension - a mechanism for agents to provide additional functionality beyond a2a spec.
---
Operations
1. Send Message - client send a message to an agent, and receive either a task that tasks the processing or a direct response message
	- Input:  request object containing the message, config and metadata
	- Output: a task object, or message


*/
