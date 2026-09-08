package gemini_responses

import (
	"fmt"
	"log/slog"
	"mime"
	"path"
	"strconv"
	"strings"

	"github.com/bytedance/sonic"
	responses2 "github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
)

// Gemini takes an attachment two ways: bytes inline, or a URI pointing at
// bytes it already holds — a Files API upload, a gs:// object on Vertex. What
// it will not do is fetch an arbitrary URL off the open web, so a link to
// someone's S3 bucket is not a thing this can carry.
//
// It is passed through as a fileUri regardless. Gemini's own error names what
// it will not accept far better than a guess made here could, and the one
// thing worse than a rejected request is the silent drop this replaced.

func nativeImageToPart(img *responses2.InputImageContent) Part {
	if id := deref(img.FileID); id != "" {
		return Part{FileData: &FilePartData{FileURI: id}}
	}

	url := deref(img.ImageURL)
	if url == "" {
		return undeliverable("an image", "it carried neither image data, a URL, nor a file id")
	}

	if strings.HasPrefix(url, "data:") {
		mimeType, data, err := utils.ParseDataURL(url)
		if err != nil {
			slog.Error("gemini: image data URL could not be parsed", slog.Any("error", err))
			return undeliverable("an image", "its inline data could not be read")
		}
		return Part{InlineData: &InlinePartData{MimeType: mimeType, Data: data}}
	}

	return Part{FileData: &FilePartData{MimeType: mimeTypeOf(url), FileURI: url}}
}

func nativeFileToPart(file *responses2.InputFileContent) Part {
	name := deref(file.FileName)

	if id := deref(file.FileID); id != "" {
		return Part{FileData: &FilePartData{MimeType: mimeTypeOf(name), FileURI: id}}
	}

	if data := deref(file.FileData); data != "" {
		if strings.HasPrefix(data, "data:") {
			mimeType, encoded, err := utils.ParseDataURL(data)
			if err != nil {
				slog.Error("gemini: file data URL could not be parsed", slog.Any("error", err))
				return undeliverable("a file", "its inline data could not be read")
			}
			return Part{InlineData: &InlinePartData{MimeType: mimeType, Data: encoded}}
		}
		// Bare base64 carries no type of its own, so the filename is all there
		// is to go on; application/pdf is the fallback because it is the
		// document type these attachments are in practice.
		mimeType := mimeTypeOf(name)
		if mimeType == "" {
			mimeType = "application/pdf"
		}
		return Part{InlineData: &InlinePartData{MimeType: mimeType, Data: data}}
	}

	if url := deref(file.FileURL); url != "" {
		return Part{FileData: &FilePartData{MimeType: mimeTypeOf(url), FileURI: url}}
	}

	return undeliverable("a file", "it carried neither file data, a URL, nor a file id")
}

func undeliverable(kind, reason string) Part {
	slog.Error("gemini: attachment dropped from the request",
		slog.String("kind", kind), slog.String("reason", reason))
	return Part{Text: utils.Ptr(responses2.UndeliverableAttachment(kind, reason))}
}

// mimeTypeOf guesses from the extension, and is content to guess nothing:
// Gemini infers the type of a file it is already holding, so an empty
// mimeType on a fileUri is normal rather than a gap.
func mimeTypeOf(nameOrURL string) string {
	ext := path.Ext(nameOrURL)
	if i := strings.IndexAny(ext, "?#"); i >= 0 {
		ext = ext[:i]
	}
	mimeType := mime.TypeByExtension(ext)
	if i := strings.IndexByte(mimeType, ';'); i >= 0 {
		mimeType = mimeType[:i]
	}
	return mimeType
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// The way back. A Gemini-shaped request arriving at the gateway carries the
// same two transports, and the reverse used to recognise only inline images —
// so a PDF, or anything held behind a fileUri, was lost on the trip back.
//
// One asymmetry is inherent and worth naming: going out, both an image URL and
// a file id become a fileUri, because Gemini has one field where the native
// shape has two. Coming back they become a file id, which is what a fileUri
// nearly always is. The URI itself is unchanged either way, so what Gemini
// receives after a round trip is identical — only which native field held it
// differs.

func partToNativeContent(part *Part) (responses2.InputContentUnion, bool) {
	if part.InlineData != nil {
		dataURL := fmt.Sprintf("data:%s;base64,%s", part.InlineData.MimeType, part.InlineData.Data)
		if isImageMime(part.InlineData.MimeType) {
			return responses2.InputContentUnion{
				OfInputImage: &responses2.InputImageContent{ImageURL: utils.Ptr(dataURL)},
			}, true
		}
		return responses2.InputContentUnion{
			OfInputFile: &responses2.InputFileContent{FileData: utils.Ptr(dataURL)},
		}, true
	}

	if part.FileData != nil && part.FileData.FileURI != "" {
		if isImageMime(part.FileData.MimeType) {
			return responses2.InputContentUnion{
				OfInputImage: &responses2.InputImageContent{FileID: utils.Ptr(part.FileData.FileURI)},
			}, true
		}
		return responses2.InputContentUnion{
			OfInputFile: &responses2.InputFileContent{FileID: utils.Ptr(part.FileData.FileURI)},
		}, true
	}

	return responses2.InputContentUnion{}, false
}

// isImageMime treats an absent type as an image. A Files API upload of a
// document carries its type; the image path is the one that legitimately has
// nothing to declare, since a file id says nothing about what it points at.
func isImageMime(mimeType string) bool {
	return mimeType == "" || strings.HasPrefix(mimeType, "image")
}

// Tool results are how an image reaches the model without anyone having typed
// a message: an MCP tool returns a screenshot, a chart, a generated picture.
//
// Gemini's functionResponse.response is JSON and so has nowhere to put one;
// the bytes go in functionResponse.parts instead, alongside it. Splitting the
// native output list across the two is what these do.

func nativeOutputToFunctionResponse(callID, name string, out responses2.FunctionCallOutputContentUnion) *FunctionResponse {
	fr := &FunctionResponse{ID: callID, Name: name, Response: map[string]any{}}

	var text []string
	if out.OfString != nil {
		text = append(text, *out.OfString)
	}

	for _, c := range out.OfList {
		switch {
		case c.OfInputText != nil:
			text = append(text, c.OfInputText.Text)
		case c.OfOutputText != nil:
			text = append(text, c.OfOutputText.Text)
		case c.OfInputImage != nil:
			fr.appendPart(nativeImageToPart(c.OfInputImage), &text)
		case c.OfInputFile != nil:
			fr.appendPart(nativeFileToPart(c.OfInputFile), &text)
		}
	}

	fr.Response["output"] = strings.Join(text, "\n")
	return fr
}

// appendPart moves one attachment part into the function response's own parts
// list. An attachment that could not be carried arrived as text, and stays
// text — the note is the whole point of it.
func (fr *FunctionResponse) appendPart(part Part, text *[]string) {
	switch {
	case part.InlineData != nil:
		fr.Parts = append(fr.Parts, FunctionResponsePart{
			InlineData: &FunctionResponseBlob{
				MimeType:    part.InlineData.MimeType,
				Data:        part.InlineData.Data,
				DisplayName: fr.nextDisplayName(),
			},
		})
	case part.FileData != nil:
		fr.Parts = append(fr.Parts, FunctionResponsePart{
			FileData: &FunctionResponseFileData{
				MimeType:    part.FileData.MimeType,
				FileURI:     part.FileData.FileURI,
				DisplayName: fr.nextDisplayName(),
			},
		})
	case part.Text != nil:
		*text = append(*text, *part.Text)
	}
}

func (fr *FunctionResponse) nextDisplayName() string {
	return "attachment_" + strconv.Itoa(len(fr.Parts)+1)
}

func functionResponseToNativeOutput(fr *FunctionResponse) responses2.FunctionCallOutputContentUnion {
	out := responses2.FunctionCallOutputContentUnion{OfList: responses2.InputContent{}}

	if text := functionResponseText(fr.Response); text != "" {
		out.OfList = append(out.OfList, responses2.InputContentUnion{
			OfInputText: &responses2.InputTextContent{Text: text},
		})
	}

	for _, part := range fr.Parts {
		switch {
		case part.InlineData != nil:
			dataURL := fmt.Sprintf("data:%s;base64,%s", part.InlineData.MimeType, part.InlineData.Data)
			if isImageMime(part.InlineData.MimeType) {
				out.OfList = append(out.OfList, responses2.InputContentUnion{
					OfInputImage: &responses2.InputImageContent{ImageURL: utils.Ptr(dataURL)},
				})
				continue
			}
			out.OfList = append(out.OfList, responses2.InputContentUnion{
				OfInputFile: &responses2.InputFileContent{
					FileData: utils.Ptr(dataURL),
					FileName: optional(part.InlineData.DisplayName),
				},
			})

		case part.FileData != nil && part.FileData.FileURI != "":
			if isImageMime(part.FileData.MimeType) {
				out.OfList = append(out.OfList, responses2.InputContentUnion{
					OfInputImage: &responses2.InputImageContent{FileID: utils.Ptr(part.FileData.FileURI)},
				})
				continue
			}
			out.OfList = append(out.OfList, responses2.InputContentUnion{
				OfInputFile: &responses2.InputFileContent{
					FileID:   utils.Ptr(part.FileData.FileURI),
					FileName: optional(part.FileData.DisplayName),
				},
			})
		}
	}

	// A plain text result stays a plain string, which is what every other
	// provider produces for one and what a reader of history expects.
	if len(out.OfList) == 1 && out.OfList[0].OfInputText != nil {
		return responses2.FunctionCallOutputContentUnion{
			OfString: utils.Ptr(out.OfList[0].OfInputText.Text),
			OfList:   responses2.InputContent{},
		}
	}

	return out
}

// functionResponseText reads the text out of a response map.
//
// "output" is the key this package writes, so it is the one to read; anything
// else is a tool speaking its own JSON, and the JSON itself is the honest
// rendering of that. Both beat the type assertion this replaced, which took an
// arbitrary map value and panicked on anything that was not a string.
func functionResponseText(response map[string]any) string {
	if len(response) == 0 {
		return ""
	}

	if v, ok := response["output"]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}

	buf, err := sonic.Marshal(response)
	if err != nil {
		slog.Error("gemini: function response could not be rendered", slog.Any("error", err))
		return ""
	}
	return string(buf)
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
