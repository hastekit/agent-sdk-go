package agui

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hastekit/agent-sdk-go/pkg/attachments"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
)

// ContentPart is the AG-UI user message text/image/document content shape.
// Media sources must reference the owned /attachments/ API, never inline data.
type ContentPart struct {
	Type     string         `json:"type"`
	Text     string         `json:"text,omitempty"`
	Source   *ContentSource `json:"source,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}
type ContentSource struct {
	Type     string `json:"type"`
	Value    string `json:"value"`
	MimeType string `json:"mimeType,omitempty"`
}

// Keep the existing Go string API while accepting the protocol's content union.
func (m Message) MarshalJSON() ([]byte, error) {
	type plain Message
	if m.ContentParts == nil {
		return json.Marshal(plain(m))
	}
	return json.Marshal(struct {
		plain
		Content []ContentPart `json:"content"`
	}{plain(m), m.ContentParts})
}
func (m *Message) UnmarshalJSON(b []byte) error {
	type plain Message
	var wire struct {
		*plain
		Content json.RawMessage `json:"content"`
	}
	*m = Message{}
	wire.plain = (*plain)(m)
	if err := json.Unmarshal(b, &wire); err != nil {
		return err
	}
	if len(wire.Content) == 0 || string(wire.Content) == "null" {
		return nil
	}
	if wire.Content[0] == '"' {
		return json.Unmarshal(wire.Content, &m.Content)
	}
	if err := json.Unmarshal(wire.Content, &m.ContentParts); err != nil {
		return err
	}
	for _, p := range m.ContentParts {
		if p.Type == "text" {
			m.Content += p.Text
		}
	}
	return nil
}

func validateMessageAttachments(ctx context.Context, namespace string, msgs []Message, store attachments.Store) error {
	for _, m := range msgs {
		if m.ContentParts != nil && m.Role != RoleUser {
			return fmt.Errorf("multipart content requires user role")
		}
		for i := range m.ContentParts {
			p := &m.ContentParts[i]
			if p.Type == "text" {
				if p.Source != nil {
					return attachments.ErrInvalid
				}
				continue
			}
			if (p.Type != "image" && p.Type != "document") || p.Source == nil || p.Source.Type != "url" {
				return attachments.ErrInvalid
			}
			if store == nil {
				return attachments.ErrUnresolved
			}
			ref, err := attachments.RefFromURL(p.Source.Value)
			if err != nil {
				return err
			}
			d, err := store.Lookup(ctx, namespace, ref)
			if err != nil {
				return err
			}
			if !attachments.SupportedMediaType(d.MediaType) || (p.Type == "image") != strings.HasPrefix(d.MediaType, "image/") {
				return attachments.ErrInvalid
			}
			// Pin the authorized immutable version; never trust client MIME/metadata.
			ref.Version = d.Version
			p.Source = &ContentSource{Type: "url", Value: attachments.URL(ref), MimeType: d.MediaType}
			p.Metadata = map[string]any{"filename": d.Filename}
		}
	}
	return nil
}

func messageContent(m Message) responses.InputContent {
	if m.ContentParts == nil {
		return responses.InputContent{{OfInputText: &responses.InputTextContent{Text: m.Content}}}
	}
	out := responses.InputContent{}
	for _, p := range m.ContentParts {
		switch p.Type {
		case "text":
			out = append(out, responses.InputContentUnion{OfInputText: &responses.InputTextContent{Text: p.Text}})
		case "image", "document":
			if p.Source == nil {
				continue
			}
			ref, err := attachments.RefFromURL(p.Source.Value)
			if err != nil {
				continue
			}
			fileID := attachments.FileID(ref)
			if p.Type == "image" {
				out = append(out, responses.InputContentUnion{OfInputImage: &responses.InputImageContent{FileID: &fileID, Detail: "auto"}})
			} else {
				name, _ := p.Metadata["filename"].(string)
				out = append(out, responses.InputContentUnion{OfInputFile: &responses.InputFileContent{FileID: &fileID, FileName: &name}})
			}
		}
	}
	return out
}

func historyContentParts(content responses.InputContent) []ContentPart {
	var parts []ContentPart
	hasFile := false
	for _, c := range content {
		if c.OfInputText != nil {
			if t := stripContextBlocks(c.OfInputText.Text); t != "" {
				parts = append(parts, ContentPart{Type: "text", Text: t})
			}
		}
		var fileID *string
		kind, name := "image", "Image"
		if c.OfInputImage != nil {
			fileID = c.OfInputImage.FileID
		}
		if c.OfInputFile != nil {
			fileID = c.OfInputFile.FileID
			kind, name = "document", "Document"
			if c.OfInputFile.FileName != nil {
				name = *c.OfInputFile.FileName
			}
		}
		if fileID != nil && attachments.IsFileID(*fileID) {
			ref, err := attachments.RefFromFileID(*fileID)
			if err != nil {
				continue
			}
			hasFile = true
			parts = append(parts, ContentPart{Type: kind, Source: &ContentSource{Type: "url", Value: attachments.URL(ref)}, Metadata: map[string]any{"filename": name}})
		}
	}
	if !hasFile {
		return nil
	}
	return parts
}
