package sdk

import "github.com/hastekit/agent-sdk-go/pkg/routines"

// NewRoutineService creates scheduled tasks targeting agents from this registry.
// Share the service between routines.NewHTTPHandler, routines.Tools, and
// a routines.Scheduler implementation. The application owns the store and scheduler lifetime.
func NewRoutineService(registry *AgentRegistry, store routines.Store) *routines.Service {
	return routines.NewService(store, &routines.AgentExecutor{Registry: registry})
}
