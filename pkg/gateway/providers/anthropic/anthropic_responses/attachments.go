package anthropic_responses

import (
	"fmt"
	"log/slog"
	"mime"
	"path"
	"strings"

	responses2 "github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
)

// Anthropic takes an attachment three ways — bytes inline, a URL it fetches
// itself, or a file already in its Files API — and the caller has already
// chosen which by picking a field. These two functions carry that choice
// across rather than recognising only one of them.
//
// Neither ever returns nothing. An attachment that cannot be carried becomes a
// line of text saying so, because the alternative is what these used to do:
// drop it, and let the model answer about a picture it was never shown.

func nativeImageToContent(img *responses2.InputImageContent) ContentUnion {
	if id := deref(img.FileID); id != "" {
		return ContentUnion{OfImage: &ImageContent{
			Source: ImageContentSource{Type: "file", FileID: utils.Ptr(id)},
		}}
	}

	url := deref(img.ImageURL)
	if url == "" {
		return undeliverable("an image", "it carried neither image data, a URL, nor a file id")
	}

	if strings.HasPrefix(url, "data:") {
		mediaType, data, err := utils.ParseDataURL(url)
		if err != nil {
			slog.Error("anthropic: image data URL could not be parsed", slog.Any("error", err))
			return undeliverable("an image", "its inline data could not be read")
		}
		return ContentUnion{OfImage: &ImageContent{
			Source: ImageContentSource{Type: "base64", Data: utils.Ptr(data), MediaType: utils.Ptr(mediaType)},
		}}
	}

	// Anthropic fetches this itself, from its own network, so the URL has to be
	// reachable without credentials. Passing it through rather than guessing at
	// that here: the API's own error says more than any check we could make.
	return ContentUnion{OfImage: &ImageContent{
		Source: ImageContentSource{Type: "url", URL: utils.Ptr(url)},
	}}
}

func nativeFileToContent(file *responses2.InputFileContent) ContentUnion {
	title := deref(file.FileName)

	if id := deref(file.FileID); id != "" {
		return ContentUnion{OfDocument: &DocumentContent{
			Source: DocumentContentSource{Type: "file", FileID: utils.Ptr(id)},
			Title:  optional(title),
		}}
	}

	if data := deref(file.FileData); data != "" {
		mediaType, encoded := fileDataParts(data, title)
		if encoded == "" {
			return undeliverable("a file", "its inline data could not be read")
		}
		return ContentUnion{OfDocument: &DocumentContent{
			Source: DocumentContentSource{Type: "base64", Data: utils.Ptr(encoded), MediaType: utils.Ptr(mediaType)},
			Title:  optional(title),
		}}
	}

	if url := deref(file.FileURL); url != "" {
		return ContentUnion{OfDocument: &DocumentContent{
			Source: DocumentContentSource{Type: "url", URL: utils.Ptr(url)},
			Title:  optional(title),
		}}
	}

	return undeliverable("a file", "it carried neither file data, a URL, nor a file id")
}

func undeliverable(kind, reason string) ContentUnion {
	slog.Error("anthropic: attachment dropped from the request",
		slog.String("kind", kind), slog.String("reason", reason))
	return ContentUnion{OfText: &TextContent{Text: responses2.UndeliverableAttachment(kind, reason)}}
}

// fileDataParts reads the media type and base64 payload out of whatever the
// caller put in FileData. A data URL carries its own type; bare base64 does
// not, so the filename is the only thing left to go on, and application/pdf is
// the fallback because it is the document type Anthropic actually accepts.
func fileDataParts(data, filename string) (mediaType, encoded string) {
	if strings.HasPrefix(data, "data:") {
		mediaType, encoded, err := utils.ParseDataURL(data)
		if err != nil {
			slog.Error("anthropic: file data URL could not be parsed", slog.Any("error", err))
			return "", ""
		}
		return mediaType, encoded
	}

	mediaType = mime.TypeByExtension(path.Ext(filename))
	if i := strings.IndexByte(mediaType, ';'); i >= 0 {
		mediaType = mediaType[:i]
	}
	if mediaType == "" {
		mediaType = "application/pdf"
	}
	return mediaType, data
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// The way back. An Anthropic-shaped request arriving at the gateway carries
// the same three transports, and the reverse used to recognise only base64 —
// so a URL or a file id survived the trip out and was lost on the trip back,
// which is the same bug in a mirror.

func imageContentToNative(img *ImageContent) responses2.InputContentUnion {
	switch img.Source.Type {
	case "url":
		if url := deref(img.Source.URL); url != "" {
			return responses2.InputContentUnion{
				OfInputImage: &responses2.InputImageContent{ImageURL: utils.Ptr(url)},
			}
		}

	case "file":
		if id := deref(img.Source.FileID); id != "" {
			return responses2.InputContentUnion{
				OfInputImage: &responses2.InputImageContent{FileID: utils.Ptr(id)},
			}
		}

	case "base64":
		// Guarded because the fields belong to this arm alone: a source that
		// names base64 without carrying it is malformed, not a reason to panic.
		mediaType, data := deref(img.Source.MediaType), deref(img.Source.Data)
		if mediaType != "" && data != "" {
			return responses2.InputContentUnion{
				OfInputImage: &responses2.InputImageContent{
					ImageURL: utils.Ptr(fmt.Sprintf("data:%s;base64,%s", mediaType, data)),
				},
			}
		}
	}

	slog.Error("anthropic: inbound image could not be read",
		slog.String("source_type", img.Source.Type))
	return responses2.InputContentUnion{
		OfInputText: &responses2.InputTextContent{
			Text: responses2.UndeliverableAttachment("an image", "its source could not be read"),
		},
	}
}

func documentContentToNative(doc *DocumentContent) responses2.InputContentUnion {
	file := &responses2.InputFileContent{FileName: optional(deref(doc.Title))}

	switch doc.Source.Type {
	case "url":
		if url := deref(doc.Source.URL); url != "" {
			file.FileURL = utils.Ptr(url)
			return responses2.InputContentUnion{OfInputFile: file}
		}

	case "file":
		if id := deref(doc.Source.FileID); id != "" {
			file.FileID = utils.Ptr(id)
			return responses2.InputContentUnion{OfInputFile: file}
		}

	case "base64", "text":
		mediaType, data := deref(doc.Source.MediaType), deref(doc.Source.Data)
		if mediaType != "" && data != "" {
			file.FileData = utils.Ptr(fmt.Sprintf("data:%s;base64,%s", mediaType, data))
			return responses2.InputContentUnion{OfInputFile: file}
		}
	}

	slog.Error("anthropic: inbound document could not be read",
		slog.String("source_type", doc.Source.Type))
	return responses2.InputContentUnion{
		OfInputText: &responses2.InputTextContent{
			Text: responses2.UndeliverableAttachment("a file", "its source could not be read"),
		},
	}
}

// Tool results carry the same attachments, and are the way an image reaches
// the model without anyone having typed a message: an MCP tool returns a
// screenshot, a chart, a generated picture. Anthropic's tool_result takes
// text, image and document blocks, so all three cross both ways.

func nativeOutputToContent(out responses2.InputContentUnion) ContentUnion {
	switch {
	case out.OfInputText != nil:
		return ContentUnion{OfText: &TextContent{Text: out.OfInputText.Text}}
	case out.OfOutputText != nil:
		return ContentUnion{OfText: &TextContent{Text: out.OfOutputText.Text}}
	case out.OfInputImage != nil:
		return nativeImageToContent(out.OfInputImage)
	case out.OfInputFile != nil:
		return nativeFileToContent(out.OfInputFile)
	}
	return ContentUnion{}
}

func toolResultContentToNative(content ContentUnion) (responses2.InputContentUnion, bool) {
	switch {
	case content.OfText != nil:
		return responses2.InputContentUnion{
			OfInputText: &responses2.InputTextContent{Text: content.OfText.Text},
		}, true
	case content.OfImage != nil:
		return imageContentToNative(content.OfImage), true
	case content.OfDocument != nil:
		return documentContentToNative(content.OfDocument), true
	}
	return responses2.InputContentUnion{}, false
}
