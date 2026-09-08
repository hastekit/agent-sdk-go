package base

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"

	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
)

// SendResponseChunk releases a producer when a consumer cancels its request.
func SendResponseChunk(ctx context.Context, out chan<- *responses.ResponseChunk, chunk *responses.ResponseChunk) bool {
	select {
	case <-ctx.Done():
		return false
	case out <- chunk:
		return true
	}
}

// StreamResponsesSSE owns body and closes it on completion, failure, or
// cancellation. A response must include a terminal completion event; EOF alone
// cannot distinguish success from a truncated HTTP response.
//
// Individual events are best-effort: an event the converter cannot decode, or
// one that is not even valid JSON, is logged and skipped. Only the stream as a
// whole fails, and only for reasons that are actually about the stream — an
// explicit provider error event, a read failure, or an end without a
// completion event. A single damaged frame therefore cannot discard the events
// around it.
func StreamResponsesSSE(ctx context.Context, body io.ReadCloser, convert func([]byte) ([]*responses.ResponseChunk, error)) chan *responses.ResponseChunk {
	out := make(chan *responses.ResponseChunk)
	go func() {
		defer close(out)
		defer body.Close()
		stop := context.AfterFunc(ctx, func() { _ = body.Close() })
		defer stop()
		fail := func(err error) { SendResponseChunk(ctx, out, responses.NewStreamError(err)) }
		dispatchOne := func(data string) bool {
			if data == "[DONE]" {
				fail(errors.New("stream ended at [DONE] without a completion event"))
				return false
			}
			streamErr, err := responses.ParseStreamError([]byte(data))
			if err != nil {
				// Keepalive frames, proxy noise and partially written events
				// all land here. None of them say the stream failed, and
				// discarding the events already delivered would turn a
				// recoverable blemish into a failed run.
				slog.WarnContext(ctx, "skipping unparseable stream event", "error", err)
				return true
			}
			if streamErr != nil {
				SendResponseChunk(ctx, out, &responses.ResponseChunk{OfError: streamErr})
				return false
			}
			chunks, err := convert([]byte(data))
			if err != nil {
				// Providers add event types and payload variants independently
				// of SDK releases. A conversion failure is not evidence that
				// the HTTP stream failed; keep reading for its terminal event.
				slog.WarnContext(ctx, "skipping unrecognized stream event", "error", err)
				return true
			}
			for _, chunk := range chunks {
				if chunk == nil {
					continue
				}
				if !SendResponseChunk(ctx, out, chunk) {
					return false
				}
				if chunk.OfResponseCompleted != nil || chunk.OfError != nil {
					return false
				}
			}
			return true
		}
		// SSE joins the data lines of one event with newlines. Providers that
		// omit the blank line between events leave several whole payloads in
		// one frame instead, and joining those yields nonsense. Fall back to
		// one event per line when the joined frame is not itself valid JSON.
		dispatch := func(lines []string) bool {
			joined := strings.Join(lines, "\n")
			if len(lines) == 1 || json.Valid([]byte(joined)) {
				return dispatchOne(joined)
			}
			for _, line := range lines {
				if !dispatchOne(line) {
					return false
				}
			}
			return true
		}
		reader := bufio.NewReader(body)
		var data []string
		for {
			line, err := reader.ReadString('\n')
			line = strings.TrimRight(line, "\r\n")
			if strings.HasPrefix(line, "data:") {
				data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			} else if line == "" && len(data) > 0 {
				if !dispatch(data) {
					return
				}
				data = nil
			}
			if err != nil {
				if len(data) > 0 && !dispatch(data) {
					return
				}
				if err == io.EOF {
					err = io.ErrUnexpectedEOF
				}
				fail(err)
				return
			}
		}
	}()
	return out
}
