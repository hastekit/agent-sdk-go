// Run a local Temporal server first: temporal server start-dev
// Then: go run ./examples/workflow/temporal
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/hastekit/agent-sdk-go/pkg/workflow"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
)

func main() {
	address := flag.String("address", client.DefaultHostPort, "Temporal server address")
	flag.Parse()
	if err := run(*address); err != nil {
		log.Fatal(err)
	}
}

func run(address string) error {
	delay, err := workflow.NewDelayNode("wait", workflow.DelayNodeConfig{Duration: time.Second})
	if err != nil {
		return err
	}
	review, err := workflow.NewHumanNode("review", workflow.HumanNodeConfig{Message: "Approve this payment?"})
	if err != nil {
		return err
	}
	finish, err := workflow.NewJavaScriptNode("finish", workflow.JavaScriptNodeConfig{Code: "return {approved: true, amount: input.amount};"})
	if err != nil {
		return err
	}
	compiled, err := workflow.NewGraph("payment").
		AddNode("wait", delay).AddNode("review", review).AddNode("finish", finish).
		AddEdge(workflow.StartNode, "wait").AddEdge("wait", "review").
		AddEdgeOnPort("review", "approved", "finish").
		AddEdgeOnPort("review", "rejected", workflow.EndNode).
		AddEdge("finish", workflow.EndNode).Compile()
	if err != nil {
		return err
	}
	executor, err := workflow.NewTemporalExecutor("payment", compiled, workflow.TemporalExecutorOptions{})
	if err != nil {
		return err
	}
	c, err := client.Dial(client.Options{HostPort: address})
	if err != nil {
		return err
	}
	defer c.Close()
	const taskQueue = "payment-workflows"
	w := worker.New(c, taskQueue, worker.Options{})
	executor.Register(w)
	if err := w.Start(); err != nil {
		return err
	}
	defer w.Stop()

	state := &workflow.Input{RunContext: map[string]any{"input": map[string]any{"amount": 150}}}
	for {
		// Each checkpoint continuation is a new Temporal execution; the logical
		// workflow RunID and completed node state are retained in Input.
		run, err := c.ExecuteWorkflow(context.Background(), client.StartWorkflowOptions{
			ID: "payment-" + uuid.NewString(), TaskQueue: taskQueue,
		}, "payment", state)
		if err != nil {
			return err
		}
		var next workflow.Input
		if err := run.Get(context.Background(), &next); err != nil {
			return err
		}
		state = &next
		if state.Pause == nil {
			fmt.Printf("Completed: %+v\n", state.RunContext["nodes"])
			return nil
		}
		fmt.Println("Paused:", state.Pause.NodeID)
		// Demo decision; a real caller persists this checkpoint and waits for a user.
		state.SetResume(state.Pause.NodeID, map[string]any{"action": "approve"})
	}
}
