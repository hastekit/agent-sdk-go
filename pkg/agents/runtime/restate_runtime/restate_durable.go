package restate_runtime

import (
	"github.com/hastekit/agent-sdk-go/pkg/agents"
	restate "github.com/restatedev/sdk-go"
)

// RestateDurableStep runs the agent loop's fire-and-forget lifecycle side
// effects (stream publishes) exactly once. The Restate handler replays from
// the top on recovery; wrapping each side effect in restate.RunVoid records it
// as a journaled step that is skipped on replay, so it executes exactly once.
// fn performs external side effects only (broker publishes), which is what
// restate.Run is intended for.
type RestateDurableStep struct {
	restateCtx restate.WorkflowContext
}

func NewRestateDurableStep(restateCtx restate.WorkflowContext) *RestateDurableStep {
	return &RestateDurableStep{restateCtx: restateCtx}
}

var _ agents.DurableStep = (*RestateDurableStep)(nil)

func (s *RestateDurableStep) Do(fn func()) {
	_ = restate.RunVoid(s.restateCtx, func(restate.RunContext) error {
		fn()
		return nil
	})
}
