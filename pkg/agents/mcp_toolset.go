package agents

import (
	"context"
)

type MCPToolset interface {
	GetName() string
	// ListTools receives the execution namespace separately from application run context.
	ListTools(ctx context.Context, namespace string, runContext map[string]any) ([]Tool, error)
}
