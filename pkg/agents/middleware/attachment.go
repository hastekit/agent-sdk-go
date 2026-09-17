package middleware

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"slices"
	"strings"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/attachments"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
)

// AttachmentMiddlewareConfig configures the middleware in both directions: Store is
// where inline tool output and generated model images are put, and Resolver is where
// an attachment reference is read back from when the model needs the bytes.
type AttachmentMiddlewareConfig struct {
	// InlineAttachments resolves file attachments into bytes for model calls.
	// By default, files are sent as plain-text references with available metadata.
	// Image resolution is controlled independently by InlineImages.
	InlineAttachments bool

	// InlineImages resolves image attachments and generated-image history into bytes.
	// Nil defaults to true; set to a pointer to false to send image references as text.
	InlineImages *bool

	Store        attachments.UploadStore
	MaxFileBytes int64 // zero: 20 MiB per decoded attachment

	// Resolver supplies metadata and, when either inline option is enabled, bytes.
	// When nil, metadata is read from Store directly; inline mode builds a resolver
	// over Store with MaxFileBytes and default cache limits. Set it to share one
	// resolver and its byte cache across agents.
	Resolver *attachments.Resolver

	// MaxInlineBytes limits the encoded attachment content one model request
	// may carry when either inline option is enabled, counting every occurrence
	// (zero: 32 MiB).
	MaxInlineBytes int64
}

// AttachmentMiddleware keeps attachment bytes out of everything durable, in both
// directions of a run.
//
// On the way in from a tool, WrapToolCall uploads inline image/file parts in
// the result's Output.OfList and replaces them with namespaced file_id values,
// so what is journaled and stored in history is the reference. It never
// inspects arbitrary text or JSON, fetches URLs, or opens tool-supplied
// filesystem paths, and an upload failure aborts the call rather than letting
// an inline result into history. Runtime adapters run it inside the step that
// ran the tool, before that step returns.
//
// On the way out to the model, WrapModelCall replaces SDK attachment references
// on a transient copy: images resolve to bytes by default, while files become
// plain-text references. InlineImages and InlineAttachments control these independently.
// This includes user input, tool results, and generated-image history.
// On the way back, generated images are uploaded and replaced with references.
// Preparation and uploads run inside the model call's own step; failures abort
// the call before crossing the model durability boundary.
//
// Register it first among an agent's middlewares, so it is outermost: a result that
// another middleware's wrap adds media to on its way back still passes through it.
// Adding this middleware to an agent is what turns attachment support on for it;
// removing it turns it off. Nothing about the LLM client changes either way.
type AttachmentMiddleware struct {
	agents.NoopMiddleware
	cfg          AttachmentMiddlewareConfig
	resolver     *attachments.Resolver
	inlineImages bool
}

var _ agents.Middleware = (*AttachmentMiddleware)(nil)

func NewAttachmentMiddleware(cfg AttachmentMiddlewareConfig) *AttachmentMiddleware {
	if cfg.MaxFileBytes <= 0 {
		cfg.MaxFileBytes = 20 << 20
	}
	inlineImages := cfg.InlineImages == nil || *cfg.InlineImages
	resolver := cfg.Resolver
	if (cfg.InlineAttachments || inlineImages) && resolver == nil && cfg.Store != nil {
		resolver = attachments.NewResolver(cfg.Store, attachments.Config{MaxFileBytes: cfg.MaxFileBytes})
	}
	return &AttachmentMiddleware{cfg: cfg, resolver: resolver, inlineImages: inlineImages}
}

// WrapModelCall prepares attachment references on a transient request copy and
// externalizes generated images before returning to the agent loop. Byte
// resolution requires a Resolver or Store. Uploading raw
// generated images always requires Store.
func (h *AttachmentMiddleware) WrapModelCall(next agents.ModelCallFunc) agents.ModelCallFunc {
	return func(ctx context.Context, call *agents.ModelCall, request *responses.Request) (*responses.Response, error) {
		namespace, sessionID := "", ""
		if call != nil {
			namespace, sessionID = call.Namespace, call.SessionID
		}
		if request != nil {
			prepared, err := h.prepareAttachmentReferences(ctx, namespace, sessionID, request)
			if err == nil && (h.inlineImages || h.cfg.InlineAttachments) {
				prepared, err = PrepareAttachments(ctx, namespace, sessionID, prepared, h.resolver, h.cfg.MaxInlineBytes)
			}
			if err != nil {
				return nil, err
			}
			request = prepared
		}
		images := &generatedImageExternalizer{
			middleware: h,
			namespace:  namespace,
			sessionID:  sessionID,
			uploaded:   make(map[[32]byte]attachments.Ref),
		}
		ctx = agents.WithModelStreamTransform(ctx, images.transformChunk)
		response, err := next(ctx, call, request)
		if err != nil {
			return response, err
		}
		return images.externalizeModelResponse(ctx, response)
	}
}

// generatedImageExternalizer belongs to one model call. Its upload map is
// shared by live done/completed chunks and the accumulated response, so one
// payload is stored once even when a provider repeats it at every layer.
type generatedImageExternalizer struct {
	middleware *AttachmentMiddleware
	namespace  string
	sessionID  string
	uploaded   map[[32]byte]attachments.Ref
}

// transformChunk removes binary previews and replaces complete generated
// images with owned references before the generic publisher can see them.
// Non-image chunks are returned verbatim, preserving ordinary streaming.
func (x *generatedImageExternalizer) transformChunk(ctx context.Context, chunk *responses.ResponseChunk) (*responses.ResponseChunk, error) {
	if chunk == nil {
		return nil, nil
	}
	if chunk.OfImageGenerationCallPartialImage != nil {
		return nil, nil
	}
	if event := chunk.OfOutputItemDone; event != nil && event.Item.Type == "image_generation_call" && event.Item.Result != nil && *event.Item.Result != "" {
		result, changed, err := x.externalizeResult(ctx, *event.Item.Result, pointerString(event.Item.OutputFormat))
		if err != nil {
			return nil, fmt.Errorf("model output item %q: %w", event.Item.Id, err)
		}
		if !changed {
			return chunk, nil
		}
		copyChunk := *chunk
		copyEvent := *event
		copyEvent.Item = event.Item
		copyEvent.Item.Result = &result
		copyChunk.OfOutputItemDone = &copyEvent
		return &copyChunk, nil
	}
	if event := chunk.OfResponseCompleted; event != nil && len(event.Response.Output) > 0 {
		output, changed, err := x.externalizeOutput(ctx, event.Response.Output)
		if err != nil {
			return nil, fmt.Errorf("completed model response: %w", err)
		}
		if !changed {
			return chunk, nil
		}
		copyChunk := *chunk
		copyEvent := *event
		copyEvent.Response = event.Response
		copyEvent.Response.Output = output
		copyChunk.OfResponseCompleted = &copyEvent
		return &copyChunk, nil
	}
	return chunk, nil
}

// externalizeModelResponse uploads every raw image_generation_call result and
// replaces it with an attachment:// reference on a copy. The result field is
// intentionally reused: it is the shared Responses shape's existing carrier,
// and adding a second SDK-only file field would make provider conversion and
// persisted history disagree. Repeated payloads across the stream and response
// share an upload through this call's generatedImageExternalizer.
func (x *generatedImageExternalizer) externalizeModelResponse(ctx context.Context, response *responses.Response) (*responses.Response, error) {
	if response == nil || len(response.Output) == 0 {
		return response, nil
	}
	output, changed, err := x.externalizeOutput(ctx, response.Output)
	if err != nil {
		return nil, err
	}
	if !changed {
		return response, nil
	}
	copy := *response
	copy.Output = output
	return &copy, nil
}

func (x *generatedImageExternalizer) externalizeOutput(ctx context.Context, input []responses.OutputMessageUnion) ([]responses.OutputMessageUnion, bool, error) {
	output := slices.Clone(input)
	changed := false
	for i := range output {
		image := output[i].OfImageGenerationCall
		if image == nil || image.Result == "" {
			continue
		}
		result, imageChanged, err := x.externalizeResult(ctx, image.Result, image.OutputFormat)
		if err != nil {
			return nil, false, fmt.Errorf("model output image %d: %w", i, err)
		}
		if !imageChanged {
			continue
		}
		copy := *image
		copy.Result = result
		output[i].OfImageGenerationCall = &copy
		changed = true
	}
	return output, changed, nil
}

func (x *generatedImageExternalizer) externalizeResult(ctx context.Context, result, format string) (string, bool, error) {
	if attachments.IsFileID(result) {
		if _, err := attachments.RefFromFileID(result); err != nil {
			return "", false, err
		}
		return result, false, nil
	}
	if x.middleware.cfg.Store == nil {
		return "", false, attachments.ErrUnresolved
	}
	data, mediaType, err := decodeGeneratedImage(ctx, result, format, x.middleware.cfg.MaxFileBytes)
	if err != nil {
		return "", false, err
	}
	key := sha256.Sum256(data)
	ref, ok := x.uploaded[key]
	if !ok {
		ref, err = x.middleware.cfg.Store.Put(ctx, x.namespace, x.sessionID, attachments.Upload{
			Filename:  generatedImageFilename(mediaType),
			MediaType: mediaType,
			Content:   bytes.NewReader(data),
		})
		if err != nil {
			return "", false, err
		}
		if ref.ID == "" {
			return "", false, attachments.ErrInvalid
		}
		x.uploaded[key] = ref
	}
	fileID := attachments.FileID(ref)
	if _, err := attachments.RefFromFileID(fileID); err != nil {
		return "", false, err
	}
	return fileID, true, nil
}

func pointerString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func decodeGeneratedImage(ctx context.Context, result, format string, maxBytes int64) ([]byte, string, error) {
	data, detected, err := decodeToolAttachment(ctx, result, maxBytes)
	if err != nil {
		return nil, "", err
	}
	if !strings.HasPrefix(detected, "image/") {
		return nil, "", fmt.Errorf("%w: generated result is not an image", attachments.ErrInvalid)
	}
	expected, err := generatedImageMediaType(format)
	if err != nil {
		return nil, "", err
	}
	if expected != "" && expected != detected {
		return nil, "", fmt.Errorf("%w: generated image format does not match content", attachments.ErrInvalid)
	}
	return data, detected, nil
}

func generatedImageMediaType(format string) (string, error) {
	format = strings.ToLower(strings.TrimSpace(format))
	format = strings.TrimPrefix(format, "image/")
	switch format {
	case "":
		return "", nil
	case "png":
		return "image/png", nil
	case "jpg", "jpeg":
		return "image/jpeg", nil
	case "webp":
		return "image/webp", nil
	case "gif":
		return "image/gif", nil
	default:
		return "", fmt.Errorf("%w: unsupported generated image format %q", attachments.ErrInvalid, format)
	}
}

func generatedImageFilename(mediaType string) string {
	switch mediaType {
	case "image/png":
		return "generated-image.png"
	case "image/jpeg":
		return "generated-image.jpg"
	case "image/webp":
		return "generated-image.webp"
	case "image/gif":
		return "generated-image.gif"
	default:
		return "generated-image"
	}
}

// WrapToolCall lets the call run, then externalizes the result into the
// call's namespace. The tool's own error goes back as it came; an upload
// failure is this middleware's, and ends the run.
func (h *AttachmentMiddleware) WrapToolCall(next agents.ToolCallFunc) agents.ToolCallFunc {
	return func(ctx context.Context, tool *agents.BaseTool, call *agents.ToolCall) (*agents.ToolCallResponse, error) {
		result, err := next(ctx, tool, call)
		if err != nil {
			return nil, err
		}
		namespace, sessionID := "", ""
		if call != nil {
			namespace, sessionID = call.Namespace, call.SessionID
		}
		return h.externalize(ctx, namespace, sessionID, result)
	}
}

// externalize uploads every inline image/file part of a result into namespace
// and replaces it with a reference, on a copy. A result with nothing inline is
// returned as it was.
func (h *AttachmentMiddleware) externalize(ctx context.Context, namespace, sessionID string, result *agents.ToolCallResponse) (*agents.ToolCallResponse, error) {
	if result == nil || result.FunctionCallOutputMessage == nil || len(result.Output.OfList) == 0 {
		return result, nil
	}
	if result.Output.OfString != nil {
		return nil, attachments.ErrInvalid
	}
	content := append(responses.InputContent(nil), result.Output.OfList...)
	changed := false
	// Repeated payloads within one result share an upload; no global raw-byte cache.
	type uploadKey struct {
		hash            [32]byte
		filename, media string
	}
	uploaded := make(map[uploadKey]attachments.Ref)
	for i, c := range content {
		if c.OfInputImage != nil && c.OfInputFile != nil {
			return nil, attachments.ErrInvalid
		}
		var source, filename string
		isImage := c.OfInputImage != nil
		switch {
		case isImage:
			img := c.OfInputImage
			if img.FileID != nil {
				if *img.FileID == "" || img.ImageURL != nil {
					return nil, attachments.ErrInvalid
				}
				if attachments.IsFileID(*img.FileID) {
					if _, err := attachments.RefFromFileID(*img.FileID); err != nil {
						return nil, err
					}
				}
				continue
			}
			if img.ImageURL == nil {
				return nil, attachments.ErrInvalid
			}
			source = *img.ImageURL
			if !strings.HasPrefix(source, "data:") {
				return nil, fmt.Errorf("%w: tool images must contain a data URI or attachment:// file_id", attachments.ErrInvalid)
			}
		case c.OfInputFile != nil:
			f := c.OfInputFile
			if f.FileID != nil {
				if *f.FileID == "" || f.FileData != nil || f.FileURL != nil {
					return nil, attachments.ErrInvalid
				}
				if attachments.IsFileID(*f.FileID) {
					if _, err := attachments.RefFromFileID(*f.FileID); err != nil {
						return nil, err
					}
				}
				continue
			}
			if f.FileData == nil || f.FileURL != nil {
				return nil, fmt.Errorf("%w: tool files must contain file_data or attachment:// file_id", attachments.ErrInvalid)
			}
			source = *f.FileData
			if f.FileName != nil {
				filename = *f.FileName
			}
		default:
			continue
		}
		if h.cfg.Store == nil {
			return nil, attachments.ErrUnresolved
		}
		data, media, err := decodeToolAttachment(ctx, source, h.cfg.MaxFileBytes)
		if err != nil {
			return nil, fmt.Errorf("tool output part %d: %w", i, err)
		}
		if isImage && !strings.HasPrefix(media, "image/") {
			return nil, attachments.ErrInvalid
		}
		if filename == "" {
			filename = "attachment"
			switch media {
			case "image/png":
				filename += ".png"
			case "image/jpeg":
				filename += ".jpg"
			case "image/webp":
				filename += ".webp"
			case "image/gif":
				filename += ".gif"
			case "application/pdf":
				filename += ".pdf"
			}
		}
		key := uploadKey{sha256.Sum256(data), filename, media}
		ref, ok := uploaded[key]
		if !ok {
			ref, err = h.cfg.Store.Put(ctx, namespace, sessionID, attachments.Upload{Filename: filename, MediaType: media, Content: bytes.NewReader(data)})
			if err != nil {
				return nil, fmt.Errorf("upload tool output part %d: %w", i, err)
			}
			if ref.ID == "" {
				return nil, attachments.ErrInvalid
			}
			uploaded[key] = ref
		}
		fileID := attachments.FileID(ref)
		if isImage {
			img := *c.OfInputImage
			img.ImageURL, img.FileID = nil, &fileID
			content[i].OfInputImage = &img
		} else {
			f := *c.OfInputFile
			f.FileData, f.FileURL, f.FileID = nil, nil, &fileID
			f.FileName = &filename
			content[i].OfInputFile = &f
		}
		changed = true
	}
	if !changed {
		return result, nil
	}
	output := *result.FunctionCallOutputMessage
	output.Output = responses.FunctionCallOutputContentUnion{OfList: content}
	copy := *result
	copy.FunctionCallOutputMessage = &output
	return &copy, nil
}

func decodeToolAttachment(ctx context.Context, source string, maxBytes int64) ([]byte, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	payload, media := source, ""
	if strings.HasPrefix(source, "data:") {
		header, body, ok := strings.Cut(source[5:], ",")
		if !ok || !strings.HasSuffix(header, ";base64") {
			return nil, "", attachments.ErrInvalid
		}
		var err error
		media, _, err = mime.ParseMediaType(strings.TrimSuffix(header, ";base64"))
		if err != nil {
			return nil, "", attachments.ErrInvalid
		}
		payload = body
	}
	// Bound both encoded input work and decoded allocations, including newlines.
	if int64(len(payload))/4 > (maxBytes+2)/3+1 {
		return nil, "", attachments.ErrTooLarge
	}
	data, err := io.ReadAll(io.LimitReader(base64.NewDecoder(base64.StdEncoding, strings.NewReader(payload)), maxBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("%w: malformed base64", attachments.ErrInvalid)
	}
	if int64(len(data)) > maxBytes {
		return nil, "", attachments.ErrTooLarge
	}
	if len(data) == 0 {
		return nil, "", attachments.ErrInvalid
	}
	detected := http.DetectContentType(data)
	if media == "" {
		media = detected
	}
	if (strings.HasPrefix(media, "image/") || media == "application/pdf") && media != detected {
		return nil, "", fmt.Errorf("%w: attachment media type does not match content", attachments.ErrInvalid)
	}
	return data, media, nil
}

// mapInputContent copies every attachment-bearing message/container before
// applying fn. Text/reasoning/tool definitions remain shared and must not be
// mutated by fn. Both regular inputs and multimodal tool results are visited.
func mapInputContent(in []responses.InputMessageUnion, fn func(responses.InputContent) (responses.InputContent, error)) ([]responses.InputMessageUnion, error) {
	out := slices.Clone(in)
	for i := range out {
		m := &out[i]
		if m.OfEasyInput != nil {
			v := *m.OfEasyInput
			c, err := fn(v.Content.OfInputMessageList)
			if err != nil {
				return nil, err
			}
			v.Content.OfInputMessageList = c
			m.OfEasyInput = &v
		}
		if m.OfInputMessage != nil {
			v := *m.OfInputMessage
			c, err := fn(v.Content)
			if err != nil {
				return nil, err
			}
			v.Content = c
			m.OfInputMessage = &v
		}
		if m.OfFunctionCallOutput != nil {
			v := *m.OfFunctionCallOutput
			c, err := fn(v.Output.OfList)
			if err != nil {
				return nil, err
			}
			v.Output.OfList = c
			m.OfFunctionCallOutput = &v
		}
	}
	return out, nil
}

// prepareAttachmentReferences reads metadata only. References remain usable by
// tools, while the original structured media stays intact for durable history.
func (h *AttachmentMiddleware) prepareAttachmentReferences(ctx context.Context, namespace, sessionID string, in *responses.Request) (*responses.Request, error) {
	changed := false
	// Repeated references share one authorized metadata lookup within this call.
	texts := make(map[string]string)
	referenceText := func(id string) (string, error) {
		if text, ok := texts[id]; ok {
			return text, nil
		}
		ref, err := attachments.RefFromFileID(id)
		if err != nil {
			return "", err
		}
		var descriptor attachments.Descriptor
		switch {
		case h.resolver != nil:
			descriptor, err = h.resolver.Lookup(ctx, namespace, sessionID, ref)
		case h.cfg.Store != nil:
			descriptor, err = h.cfg.Store.Lookup(ctx, namespace, sessionID, ref)
		}
		if err != nil {
			return "", fmt.Errorf("attachment metadata %q: %w", id, err)
		}
		// Show the canonical tool reference and only an explicitly configured mount path.
		info := struct {
			FileID           string `json:"file_id"`
			MountPath        string `json:"mount_path,omitempty"`
			OriginalFilename string `json:"original_filename,omitempty"`
			MediaType        string `json:"mime_type,omitempty"`
			Size             *int64 `json:"size_bytes,omitempty"`
		}{FileID: attachments.FileID(ref), MountPath: descriptor.MountPath}
		if h.resolver != nil || h.cfg.Store != nil {
			info.OriginalFilename, info.MediaType, info.Size = descriptor.Filename, descriptor.MediaType, &descriptor.Size
		}
		metadata, err := json.Marshal(info)
		if err != nil {
			return "", err
		}
		text := "Attached file: " + string(metadata) + "\nPass file_id to attachment tools. Use mount_path for shell commands only when provided, quoting the path. File contents are not included."
		texts[id] = text
		return text, nil
	}
	mapped, err := mapInputContent(in.Input.OfInputMessageList, func(content responses.InputContent) (responses.InputContent, error) {
		out := slices.Clone(content)
		for i, c := range content {
			var id string
			if img := c.OfInputImage; img != nil && img.FileID != nil && attachments.IsFileID(*img.FileID) {
				if img.ImageURL != nil || c.OfInputFile != nil || c.OfInputText != nil || c.OfOutputText != nil {
					return nil, attachments.ErrInvalid
				}
				if h.inlineImages {
					continue
				}
				id = *img.FileID
			}
			if file := c.OfInputFile; file != nil && file.FileID != nil && attachments.IsFileID(*file.FileID) {
				if file.FileURL != nil || file.FileData != nil || c.OfInputImage != nil || c.OfInputText != nil || c.OfOutputText != nil {
					return nil, attachments.ErrInvalid
				}
				if h.cfg.InlineAttachments {
					continue
				}
				id = *file.FileID
			}
			if id == "" {
				continue
			}
			text, err := referenceText(id)
			if err != nil {
				return nil, err
			}
			out[i] = responses.InputContentUnion{OfInputText: &responses.InputTextContent{Text: text}}
			changed = true
		}
		return out, nil
	})
	if err != nil {
		return nil, err
	}
	for i, m := range mapped {
		if image := m.OfImageGenerationCall; !h.inlineImages && image != nil && attachments.IsFileID(image.Result) {
			text, err := referenceText(image.Result)
			if err != nil {
				return nil, err
			}
			mapped[i] = responses.InputMessageUnion{OfEasyInput: &responses.EasyMessage{
				Role: constants.RoleAssistant, Content: responses.EasyInputContentUnion{OfString: &text},
			}}
			changed = true
		}
	}
	if !changed {
		return in, nil
	}
	out := *in
	out.Input.OfInputMessageList = mapped
	return &out, nil
}

// PrepareAttachments produces a transient copy with every SDK attachment file_id resolved to
// inline data, read under namespace. Only the returned request may be passed to
// a provider; retain the original for history, retries and traces. A request
// carrying no references is returned as it was, so the common case costs no
// copy. maxInlineBytes limits encoded attachment content across all occurrences
// in the request (zero: 32 MiB). Provider-specific request limits still apply.
func PrepareAttachments(ctx context.Context, namespace, sessionID string, in *responses.Request, resolver *attachments.Resolver, maxInlineBytes int64) (*responses.Request, error) {
	if maxInlineBytes <= 0 {
		maxInlineBytes = 32 << 20
	}
	out := *in
	var total int64
	changed := false
	type resolved struct {
		base64 string
		uri    string
		desc   attachments.Descriptor
	}
	resolvedFiles := make(map[attachments.Ref]resolved)
	resolve := func(ref attachments.Ref) (resolved, error) {
		if v, ok := resolvedFiles[ref]; ok {
			return v, nil
		}
		blob, err := resolver.Resolve(ctx, namespace, sessionID, ref)
		if err != nil {
			return resolved{}, err
		}
		d := blob.Descriptor()
		if d.Size > maxInlineBytes/4*3 {
			return resolved{}, attachments.ErrTooLarge
		}
		var b strings.Builder
		enc := base64.NewEncoder(base64.StdEncoding, &b)
		if _, err := blob.WriteTo(enc); err != nil {
			return resolved{}, err
		}
		if err := enc.Close(); err != nil {
			return resolved{}, err
		}
		encoded := b.String()
		v := resolved{base64: encoded, uri: "data:" + d.MediaType + ";base64," + encoded, desc: d}
		resolvedFiles[ref] = v
		return v, nil
	}
	mapped, err := mapInputContent(in.Input.OfInputMessageList, func(content responses.InputContent) (responses.InputContent, error) {
		cpy := slices.Clone(content)
		for i, c := range cpy {
			// Existing inline inputs remain supported, but also consume the
			// aggregate transport budget when mixed with owned references.
			if img := c.OfInputImage; img != nil && img.FileID == nil && img.ImageURL != nil && strings.HasPrefix(*img.ImageURL, "data:") {
				total += int64(len(*img.ImageURL))
			}
			if f := c.OfInputFile; f != nil && f.FileID == nil && f.FileData != nil {
				total += int64(len(*f.FileData))
			}
			if total > maxInlineBytes {
				return nil, attachments.ErrTooLarge
			}

			if img := c.OfInputImage; img != nil && img.FileID != nil && attachments.IsFileID(*img.FileID) {
				if img.ImageURL != nil {
					return nil, fmt.Errorf("%w: image has multiple sources", attachments.ErrInvalid)
				}
				ref, err := attachments.RefFromFileID(*img.FileID)
				if err != nil {
					return nil, err
				}
				v, err := resolve(ref)
				if err != nil {
					return nil, err
				}
				if !strings.HasPrefix(v.desc.MediaType, "image/") {
					return nil, fmt.Errorf("%w: expected image media type, got %s", attachments.ErrInvalid, v.desc.MediaType)
				}
				total += int64(len(v.uri))
				if total > maxInlineBytes {
					return nil, attachments.ErrTooLarge
				}
				copy := *img
				copy.FileID = nil
				copy.ImageURL = &v.uri
				if copy.Detail == "" {
					copy.Detail = "auto"
				}
				cpy[i].OfInputImage = &copy
				changed = true
			}
			if f := c.OfInputFile; f != nil && f.FileID != nil && attachments.IsFileID(*f.FileID) {
				if f.FileURL != nil || f.FileData != nil {
					return nil, fmt.Errorf("%w: file has multiple sources", attachments.ErrInvalid)
				}
				ref, err := attachments.RefFromFileID(*f.FileID)
				if err != nil {
					return nil, err
				}
				v, err := resolve(ref)
				if err != nil {
					return nil, err
				}
				total += int64(len(v.uri))
				if total > maxInlineBytes {
					return nil, attachments.ErrTooLarge
				}
				copy := *f
				copy.FileID = nil
				copy.FileData = &v.uri
				if copy.FileName == nil {
					name := v.desc.Filename
					copy.FileName = &name
				}
				cpy[i].OfInputFile = &copy
				changed = true
			}
		}
		return cpy, nil
	})
	if err != nil {
		return nil, err
	}
	// Generated images are provider output items that can appear again as
	// input history. Persisted history carries the owned reference in Result;
	// providers receive ordinary image content on this transient copy, without
	// replaying a provider-owned generation-call ID (which may not be stored).
	for i := range mapped {
		image := mapped[i].OfImageGenerationCall
		if image == nil || image.Result == "" {
			continue
		}
		if !attachments.IsFileID(image.Result) {
			total += int64(len(image.Result))
			if total > maxInlineBytes {
				return nil, attachments.ErrTooLarge
			}
			continue
		}
		ref, err := attachments.RefFromFileID(image.Result)
		if err != nil {
			return nil, err
		}
		v, err := resolve(ref)
		if err != nil {
			return nil, err
		}
		if !strings.HasPrefix(v.desc.MediaType, "image/") {
			return nil, fmt.Errorf("%w: expected image media type, got %s", attachments.ErrInvalid, v.desc.MediaType)
		}
		total += int64(len(v.uri))
		if total > maxInlineBytes {
			return nil, attachments.ErrTooLarge
		}
		mapped[i] = responses.InputMessageUnion{OfEasyInput: &responses.EasyMessage{
			Role: constants.RoleUser,
			Content: responses.EasyInputContentUnion{OfInputMessageList: responses.InputContent{
				{OfInputText: &responses.InputTextContent{Text: "Previously generated image. Attachment file_id: " + attachments.FileID(ref)}},
				{OfInputImage: &responses.InputImageContent{ImageURL: &v.uri, Detail: "auto"}},
			}},
		}}
		changed = true
	}
	if !changed {
		return in, nil
	}
	out.Input.OfInputMessageList = mapped
	return &out, nil
}
