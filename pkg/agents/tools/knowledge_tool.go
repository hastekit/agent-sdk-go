package tools

import (
	"context"

	"github.com/bytedance/sonic"
	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/knowledge/vectorstores"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
)

type KnowledgePersistence interface {
	Search(ctx context.Context, query string, limit int) ([]vectorstores.SearchResult, error)
}

type KnowledgeTool struct {
	*agents.BaseTool
	knowledgePersistence KnowledgePersistence
	name                 string
	limit                int
}

type KnowledgeSearchInput struct {
	Query string `json:"query"`
}

func NewKnowledgeTool(svc KnowledgePersistence, name string, description string, limit int) *KnowledgeTool {
	return &KnowledgeTool{
		BaseTool: &agents.BaseTool{
			ToolUnion: responses.ToolUnion{
				OfFunction: &responses.FunctionTool{
					Name:        name,
					Description: utils.Ptr(description),
					Parameters: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"query": map[string]any{
								"type":        "string",
								"description": "Search query for searching the knowledge base",
							},
						},
						"required": []string{"query"},
					},
				},
			},
			RequiresApproval: false,
		},
		knowledgePersistence: svc,
		name:                 name,
		limit:                limit,
	}
}

func (t *KnowledgeTool) Execute(ctx context.Context, params *agents.ToolCall) (*agents.ToolCallResponse, error) {
	var in KnowledgeSearchInput
	err := sonic.Unmarshal([]byte(params.Arguments), &in)
	if err != nil {
		return nil, err
	}

	results, err := t.knowledgePersistence.Search(ctx, in.Query, t.limit)
	if err != nil {
		return nil, err
	}

	buf, err := sonic.Marshal(results)
	if err != nil {
		return nil, err
	}

	return &agents.ToolCallResponse{
		FunctionCallOutputMessage: &responses.FunctionCallOutputMessage{
			ID:     params.ID,
			CallID: params.CallID,
			Output: responses.FunctionCallOutputContentUnion{
				OfString: utils.Ptr(string(buf)),
			},
		},
	}, nil
}
