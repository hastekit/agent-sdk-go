package agents

import (
	"context"
	"slices"
	"sort"
	"strings"

	"github.com/bytedance/sonic"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
)

type ToolSearchTool struct {
	*BaseTool
	deferredTools []Tool
}

type ToolSearchInput struct {
	Query      string `json:"query"`
	MaxResults int    `json:"max_results"`
}

func NewToolSearchTool(deferredTools []Tool) *ToolSearchTool {
	return &ToolSearchTool{
		BaseTool: &BaseTool{
			ToolUnion: responses.ToolUnion{
				OfFunction: &responses.FunctionTool{
					Name:        "ToolSearch",
					Description: utils.Ptr("Fetches full schema definitions for deferred tools and activates them so they can be called."),
					Parameters: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"query": map[string]string{
								"type":        "string",
								"description": "Query to find deferred tools. Use \"select:<tool_name>\" for direct selection, or keywords to search.",
							},
							"max_results": map[string]any{
								"type":        "integer",
								"description": "Maximum number of results to return. Default is 5.",
							},
						},
						"required": []string{"query"},
					},
					Strict: nil,
				},
			},
			RequiresApproval: false,
			Deferred:         false,
		},
		deferredTools: deferredTools,
	}
}

func (t *ToolSearchTool) Execute(ctx context.Context, params *ToolCall) (*ToolCallResponse, error) {
	var in ToolSearchInput
	err := sonic.Unmarshal([]byte(params.Arguments), &in)
	if err != nil {
		return nil, err
	}

	var toolNames []string
	if strings.HasPrefix(in.Query, "select:") {
		// Absolute selection: activate exactly the named tools.
		requested := strings.Split(strings.TrimPrefix(in.Query, "select:"), ",")
		for _, tool := range t.deferredTools {
			if schema := tool.Tool(ctx); schema.OfFunction != nil && slices.Contains(requested, schema.OfFunction.Name) {
				toolNames = append(toolNames, schema.OfFunction.Name)
			}
		}
	} else {
		// Keyword search: rank deferred tools by relevance to the query.
		// Without this a plain-keyword query (which the tool's own schema
		// invites) would match nothing and silently activate no tools.
		toolNames = t.keywordSearch(ctx, in.Query, in.MaxResults)
	}

	var output string
	if len(toolNames) == 0 {
		output = "No matching deferred tools found for query: " + in.Query
	} else {
		output = "Activated tools: " + strings.Join(toolNames, ",")
	}

	return &ToolCallResponse{
		FunctionCallOutputMessage: &responses.FunctionCallOutputMessage{
			ID:     params.ID,
			CallID: params.CallID,
			Output: responses.FunctionCallOutputContentUnion{
				OfString: utils.Ptr(output),
			},
		},
		StateUpdates: map[string]string{
			"activated_deferred_tools": strings.Join(toolNames, ","),
		},
	}, nil
}

// keywordSearch ranks deferred tools by how many query terms appear in
// their name or description, best first, and returns up to maxResults
// names (default 5). Case-insensitive substring match — enough for a
// model that queries by a tool's name or a word from its description.
func (t *ToolSearchTool) keywordSearch(ctx context.Context, query string, maxResults int) []string {
	if maxResults <= 0 {
		maxResults = 5
	}

	terms := strings.Fields(strings.ToLower(query))
	if len(terms) == 0 {
		return nil
	}

	type scored struct {
		name  string
		score int
	}
	var matches []scored
	for _, tool := range t.deferredTools {
		schema := tool.Tool(ctx)
		if schema.OfFunction == nil {
			continue
		}
		haystack := strings.ToLower(schema.OfFunction.Name)
		if schema.OfFunction.Description != nil {
			haystack += " " + strings.ToLower(*schema.OfFunction.Description)
		}
		score := 0
		for _, term := range terms {
			if strings.Contains(haystack, term) {
				score++
			}
		}
		if score > 0 {
			matches = append(matches, scored{name: schema.OfFunction.Name, score: score})
		}
	}

	// Stable sort keeps declaration order among equally-scored tools.
	sort.SliceStable(matches, func(i, j int) bool { return matches[i].score > matches[j].score })

	names := make([]string, 0, len(matches))
	for _, m := range matches {
		names = append(names, m.name)
	}
	if len(names) > maxResults {
		names = names[:maxResults]
	}
	return names
}
