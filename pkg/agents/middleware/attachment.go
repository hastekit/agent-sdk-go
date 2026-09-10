package middleware

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"net/http"
	"slices"
	"strings"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/attachments"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
)

// AttachmentMiddlewareConfig configures the middleware in both directions: Store is
// where a tool's inline output is put, and Resolver is where an attachment file_id is
// read back from when the model needs the bytes.
type AttachmentMiddlewareConfig struct {
	Store        attachments.UploadStore
	MaxFileBytes int64 // zero: 20 MiB per decoded attachment

	// Resolver reads owned references back for the model. Leave it nil to
	// have one built over Store with MaxFileBytes and default cache limits;
	// set it to share one resolver — and its byte cache — across agents.
	Resolver *attachments.Resolver

	// MaxInlineBytes limits the encoded attachment content one model request
	// may carry, counting every occurrence (zero: 32 MiB).
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
// On the way out to the model, WrapModelCall resolves every SDK attachment ID in the
// request — user input, tool results, and a middleware's appended notes alike — into
// the inline data a provider accepts, on a copy the loop never keeps. It runs
// inside the model call's own step, after the request has crossed into it, so
// the bytes go to the provider and nowhere else. A reference that cannot be
// resolved fails the call, and the provider is never contacted.
//
// Register it first among an agent's middlewares, so it is outermost: a result that
// another middleware's wrap adds media to on its way back still passes through it.
// Adding this middleware to an agent is what turns attachment support on for it;
// removing it turns it off. Nothing about the LLM client changes either way.
type AttachmentMiddleware struct {
	agents.NoopMiddleware
	cfg      AttachmentMiddlewareConfig
	resolver *attachments.Resolver
}

var _ agents.Middleware = (*AttachmentMiddleware)(nil)

func NewAttachmentMiddleware(cfg AttachmentMiddlewareConfig) *AttachmentMiddleware {
	if cfg.MaxFileBytes <= 0 {
		cfg.MaxFileBytes = 20 << 20
	}
	resolver := cfg.Resolver
	if resolver == nil && cfg.Store != nil {
		resolver = attachments.NewResolver(cfg.Store, attachments.Config{MaxFileBytes: cfg.MaxFileBytes})
	}
	return &AttachmentMiddleware{cfg: cfg, resolver: resolver}
}

// WrapModelCall hands next the request with its file references resolved into
// inline data, on a transient copy, read under the call's namespace. A request
// carrying no references goes through as it was. With neither Resolver nor
// Store configured, a request that does carry one fails with
// attachments.ErrUnresolved: silently sending the provider a reference it
// cannot read would fail later and less clearly.
func (h *AttachmentMiddleware) WrapModelCall(next agents.ModelCallFunc) agents.ModelCallFunc {
	return func(ctx context.Context, call *agents.ModelCall, request *responses.Request) (*responses.Response, error) {
		if request != nil {
			prepared, err := PrepareAttachments(ctx, call.Namespace, request, h.resolver, h.cfg.MaxInlineBytes)
			if err != nil {
				return nil, err
			}
			request = prepared
		}
		return next(ctx, call, request)
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
		namespace := ""
		if call != nil {
			namespace = call.Namespace
		}
		return h.externalize(ctx, namespace, result)
	}
}

// externalize uploads every inline image/file part of a result into namespace
// and replaces it with a reference, on a copy. A result with nothing inline is
// returned as it was.
func (h *AttachmentMiddleware) externalize(ctx context.Context, namespace string, result *agents.ToolCallResponse) (*agents.ToolCallResponse, error) {
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
		if isImage && (!strings.HasPrefix(media, "image/") || !attachments.SupportedMediaType(media)) {
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
			ref, err = h.cfg.Store.Put(ctx, namespace, attachments.Upload{Filename: filename, MediaType: media, Content: bytes.NewReader(data)})
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

// PrepareAttachments produces a transient copy with every SDK attachment file_id resolved to
// inline data, read under namespace. Only the returned request may be passed to
// a provider; retain the original for history, retries and traces. A request
// carrying no references is returned as it was, so the common case costs no
// copy. maxInlineBytes limits encoded attachment content across all occurrences
// in the request (zero: 32 MiB). Provider-specific request limits still apply.
func PrepareAttachments(ctx context.Context, namespace string, in *responses.Request, resolver *attachments.Resolver, maxInlineBytes int64) (*responses.Request, error) {
	if maxInlineBytes <= 0 {
		maxInlineBytes = 32 << 20
	}
	out := *in
	var total int64
	changed := false
	type resolved struct {
		uri  string
		desc attachments.Descriptor
	}
	resolvedFiles := make(map[attachments.Ref]resolved)
	resolve := func(ref attachments.Ref) (resolved, error) {
		if v, ok := resolvedFiles[ref]; ok {
			return v, nil
		}
		blob, err := resolver.Resolve(ctx, namespace, ref)
		if err != nil {
			return resolved{}, err
		}
		d := blob.Descriptor()
		if d.Size > maxInlineBytes/4*3 {
			return resolved{}, attachments.ErrTooLarge
		}
		var b strings.Builder
		b.WriteString("data:" + d.MediaType + ";base64,")
		enc := base64.NewEncoder(base64.StdEncoding, &b)
		if _, err := blob.WriteTo(enc); err != nil {
			return resolved{}, err
		}
		if err := enc.Close(); err != nil {
			return resolved{}, err
		}
		v := resolved{b.String(), d}
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
				switch v.desc.MediaType {
				case "image/png", "image/jpeg", "image/gif", "image/webp":
				default:
					return nil, fmt.Errorf("%w: unsupported image media type %s", attachments.ErrInvalid, v.desc.MediaType)
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
	if !changed {
		return in, nil
	}
	out.Input.OfInputMessageList = mapped
	return &out, nil
}
