package mcpclient

import (
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func mcpToolResult(call *agents.ToolCall, content []mcp.Content) (*agents.ToolCallResponse, error) {
	var parts responses.InputContent
	for _, c := range content {
		switch c := c.(type) {
		case *mcp.TextContent:
			parts = append(parts, responses.InputContentUnion{OfInputText: &responses.InputTextContent{Text: c.Text}})
		case *mcp.ImageContent:
			media := c.MIMEType
			if media == "" {
				media = http.DetectContentType(c.Data)
			}
			uri := "data:" + media + ";base64," + base64.StdEncoding.EncodeToString(c.Data)
			parts = append(parts, responses.InputContentUnion{OfInputImage: &responses.InputImageContent{ImageURL: &uri, Detail: "auto"}})
		case *mcp.EmbeddedResource:
			if c.Resource == nil {
				continue
			}
			resource := c.Resource
			if resource.Text != "" || resource.Blob == nil {
				parts = append(parts, responses.InputContentUnion{OfInputText: &responses.InputTextContent{Text: resource.Text}})
			}
			if resource.Blob == nil {
				continue
			}
			media := resource.MIMEType
			if media == "" {
				media = http.DetectContentType(resource.Blob)
			}
			uri := "data:" + media + ";base64," + base64.StdEncoding.EncodeToString(resource.Blob)
			if strings.HasPrefix(media, "image/") {
				parts = append(parts, responses.InputContentUnion{OfInputImage: &responses.InputImageContent{ImageURL: &uri, Detail: "auto"}})
				continue
			}
			filename := "attachment"
			if u, err := url.Parse(resource.URI); err == nil {
				if name := path.Base(u.Path); name != "" && name != "/" && name != "." {
					filename = name
				}
			}
			parts = append(parts, responses.InputContentUnion{OfInputFile: &responses.InputFileContent{FileData: &uri, FileName: &filename}})
		}
	}
	if len(parts) == 0 {
		return nil, errors.New("missing mcp tool result")
	}
	output := responses.FunctionCallOutputContentUnion{OfList: parts}
	if len(parts) == 1 && parts[0].OfInputText != nil {
		output = responses.FunctionCallOutputContentUnion{OfString: utils.Ptr(parts[0].OfInputText.Text)}
	}
	return &agents.ToolCallResponse{FunctionCallOutputMessage: &responses.FunctionCallOutputMessage{ID: call.ID, CallID: call.CallID, Output: output}}, nil
}
