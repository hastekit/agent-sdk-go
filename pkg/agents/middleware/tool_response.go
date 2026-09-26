package middleware

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/attachments"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
)

// ToolResponseMiddlewareConfig limits model-visible tool output, excluding control fields.
type ToolResponseMiddlewareConfig struct {
	// Store is optional. Without it, oversized output is discarded after extracting a preview.
	Store attachments.UploadStore

	// MaxResponseBytes bounds text bytes or the JSON encoding of multipart output.
	// Zero or negative values default to 32 KiB. The replacement must also fit.
	MaxResponseBytes int

	// MaxSummaryBytes bounds the excerpt, which is reduced further to fit file metadata.
	// Zero or negative values default to 2 KiB. No additional model call is made.
	MaxSummaryBytes int
}

// ToolResponseMiddleware bounds oversized results before they enter history or a durable journal.
// With a store it saves the full output; without one it returns a preview and retrieval guidance.
// Only Output is replaced, on a copy; IDs, state updates, interrupts and task metadata survive.
// Register before AttachmentMiddleware so inline media is externalized before the size check.
// Upload or metadata failures abort the call rather than returning the oversized payload.
type ToolResponseMiddleware struct {
	agents.NoopMiddleware
	cfg ToolResponseMiddlewareConfig
}

var _ agents.Middleware = (*ToolResponseMiddleware)(nil)

// NewToolResponseMiddleware creates a limiter shared by an agent's tool executions.
func NewToolResponseMiddleware(cfg ToolResponseMiddlewareConfig) *ToolResponseMiddleware {
	// Apply finite defaults without requiring an attachment store for small responses.
	if cfg.MaxResponseBytes <= 0 {
		cfg.MaxResponseBytes = 32 << 10
	}
	if cfg.MaxSummaryBytes <= 0 {
		cfg.MaxSummaryBytes = 2 << 10
	}
	cfg.MaxSummaryBytes = min(cfg.MaxSummaryBytes, cfg.MaxResponseBytes)
	return &ToolResponseMiddleware{cfg: cfg}
}

// WrapToolCall replaces only successful oversized results; tool errors pass through unchanged.
func (m *ToolResponseMiddleware) WrapToolCall(next agents.ToolCallFunc) agents.ToolCallFunc {
	return func(ctx context.Context, tool *agents.BaseTool, call *agents.ToolCall) (*agents.ToolCallResponse, error) {
		// Leave tool failures and empty results to the normal execution pipeline.
		result, err := next(ctx, tool, call)
		if err != nil || result == nil || result.FunctionCallOutputMessage == nil {
			return result, err
		}

		// Serialize only the model-visible content; control fields must remain inline.
		data, mediaType, extension, err := toolResponseContent(result.Output)
		if err != nil {
			return nil, err
		}
		if len(data) <= m.cfg.MaxResponseBytes {
			return result, nil
		}

		// Stop cancelled executions before preparing a replacement or writing an attachment.
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		// Without storage, be explicit that the omitted content was not saved anywhere.
		info := toolResponseReplacement{
			MediaType:    mediaType,
			Size:         int64(len(data)),
			Summary:      toolResponseSummary(result.Output, len(data), m.cfg.MaxSummaryBytes),
			Instructions: "Response exceeded the size limit and was not saved. Request fewer results, use pagination or filtering, or use another tool to read the source in smaller sections.",
		}

		// Storage is optional, but configured stores must receive an explicit execution scope.
		if m.cfg.Store != nil {
			if call == nil || call.Namespace == "" || call.SessionID == "" {
				return nil, fmt.Errorf("offload tool response requires namespace and session: %w", attachments.ErrDenied)
			}

			// Derive a stable filename from the tool call, escaping any path separators.
			if call.FunctionCallMessage == nil || call.CallID == "" {
				return nil, fmt.Errorf("offload tool response requires a tool call ID: %w", attachments.ErrInvalid)
			}
			ref, err := m.cfg.Store.Put(ctx, call.Namespace, call.SessionID, attachments.Upload{
				Filename:  "tool-response-" + url.PathEscape(call.CallID) + extension,
				MediaType: mediaType, Content: bytes.NewReader(data),
			})
			if err != nil {
				return nil, fmt.Errorf("upload tool response: %w", err)
			}
			fileID := attachments.FileID(ref)
			if _, err := attachments.RefFromFileID(fileID); err != nil {
				return nil, fmt.Errorf("uploaded tool response reference: %w", err)
			}

			// Read authorized metadata, including any configured model-visible mount path.
			descriptor, err := m.cfg.Store.Lookup(ctx, call.Namespace, call.SessionID, ref)
			if err != nil {
				return nil, fmt.Errorf("uploaded tool response metadata: %w", err)
			}
			info.FileID, info.MountPath, info.Filename = fileID, descriptor.MountPath, descriptor.Filename
			info.MediaType, info.Size = descriptor.MediaType, descriptor.Size
			info.Instructions = "Full tool response saved to file. Use file_id with attachment tools, or mount_path with shell tools when provided. Read only the sections needed."
		}

		// Use text metadata so attachment hydration cannot reinsert an offloaded payload.
		replacement, err := info.fit(m.cfg.MaxResponseBytes)
		if err != nil {
			return nil, err
		}

		// Preserve the tool's control data and original result without mutating either.
		out, message := *result, *result.FunctionCallOutputMessage
		message.Output = responses.FunctionCallOutputContentUnion{OfString: &replacement}
		out.FunctionCallOutputMessage = &message
		return &out, nil
	}
}

// toolResponseContent preserves raw strings and encodes multipart results as a JSON array.
func toolResponseContent(output responses.FunctionCallOutputContentUnion) ([]byte, string, string, error) {
	// Reject ambiguous output instead of silently discarding a populated branch.
	if output.OfString != nil && output.OfList != nil {
		return nil, "", "", fmt.Errorf("tool response has multiple content branches: %w", attachments.ErrInvalid)
	}

	// JSON returned as a string remains byte-for-byte intact in its uploaded file.
	if output.OfString != nil {
		data := []byte(*output.OfString)
		if json.Valid(data) {
			return data, "application/json", ".json", nil
		}
		return data, "text/plain", ".txt", nil
	}
	if output.OfList == nil {
		return nil, "", "", nil
	}

	// Retain text, media and file references in their original multipart structure.
	data, err := json.Marshal(output.OfList)
	if err != nil {
		return nil, "", "", fmt.Errorf("encode tool response: %w", err)
	}
	return data, "application/json", ".json", nil
}

// toolResponseSummary describes the payload and previews text without copying encoded media.
func toolResponseSummary(output responses.FunctionCallOutputContentUnion, size, limit int) string {
	var summary strings.Builder
	if output.OfString != nil {
		// Label the extract explicitly; it is a preview rather than a semantic model summary.
		fmt.Fprintf(&summary, "Response: %d bytes. Text preview:\n", size)
		summary.WriteString(toolResponsePrefix(*output.OfString, max(0, limit-summary.Len())))
		return toolResponsePrefix(summary.String(), limit)
	}

	// Count content types so non-text results still have a useful bounded description.
	var texts, images, files int
	for _, part := range output.OfList {
		if part.OfInputText != nil || part.OfOutputText != nil {
			texts++
		}
		if part.OfInputImage != nil {
			images++
		}
		if part.OfInputFile != nil {
			files++
		}
	}
	fmt.Fprintf(&summary, "Response: %d bytes; %d text parts, %d images, %d files. Text preview:\n", size, texts, images, files)

	// Extract only text parts; base64 data and file URLs never enter the preview.
	for _, part := range output.OfList {
		if summary.Len() >= limit {
			break
		}
		var text string
		if part.OfInputText != nil {
			text = part.OfInputText.Text
		} else if part.OfOutputText != nil {
			text = part.OfOutputText.Text
		}
		if text != "" {
			summary.WriteString(toolResponsePrefix(text, limit-summary.Len()))
			summary.WriteByte('\n')
		}
	}
	return toolResponsePrefix(summary.String(), limit)
}

// toolResponsePrefix truncates at a UTF-8 boundary and keeps malformed source bytes out of summaries.
func toolResponsePrefix(text string, limit int) string {
	// Bound the input before normalizing it so a large response does not become a large preview.
	end := min(len(text), limit)
	for end > 0 && end < len(text) && !utf8.RuneStart(text[end]) {
		end--
	}
	return strings.ToValidUTF8(text[:end], "?")
}

// toolResponseReplacement contains a preview and optional public file metadata, never storage keys.
type toolResponseReplacement struct {
	FileID       string `json:"file_id,omitempty"`
	MountPath    string `json:"mount_path,omitempty"`
	Filename     string `json:"original_filename,omitempty"`
	MediaType    string `json:"mime_type"`
	Size         int64  `json:"size_bytes"`
	Summary      string `json:"summary"`
	Instructions string `json:"instructions"`
}

// fit accounts for JSON escaping when fitting the summary alongside required response metadata.
func (info toolResponseReplacement) fit(limit int) (string, error) {
	// Reject limits too small for the retrieval guidance and any file reference.
	summary := info.Summary
	info.Summary = ""
	minimal, err := json.Marshal(info)
	if err != nil {
		return "", err
	}
	if len(minimal) > limit {
		return "", fmt.Errorf("tool response limit %d cannot fit %d bytes of response metadata: %w", limit, len(minimal), attachments.ErrTooLarge)
	}

	// Find the largest UTF-8 prefix whose escaped representation fits the remaining budget.
	best := minimal
	low, high := 0, len(summary)
	for low <= high {
		middle := low + (high-low)/2
		info.Summary = toolResponsePrefix(summary, middle)
		encoded, err := json.Marshal(info)
		if err != nil {
			return "", err
		}
		if len(encoded) <= limit {
			best, low = encoded, middle+1
		} else {
			high = middle - 1
		}
	}
	return string(best), nil
}
