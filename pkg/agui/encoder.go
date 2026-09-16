package agui

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
)

// Encoder writes AG-UI events as SSE frames onto an io.Writer. The
// wire format follows the upstream HttpAgent transport:
//
//	id: <monotonic counter>
//	event: <EVENT_TYPE>
//	data: <event JSON>
//	\n
//
// AG-UI handlers use EncodeWithID with stable replay cursors. The standalone
// Encode method uses a connection-local counter and is not resumable by itself.
// The `id` line carries that cursor; the
// `event` line lets dispatchers route without parsing JSON; the
// `data` line is one line because all our event JSON serialises
// without embedded newlines (json.Marshal escapes them). If that ever
// changes, splitData below handles multi-line bodies per the SSE
// spec.
type Encoder struct {
	w       io.Writer
	flusher http.Flusher
	seq     atomic.Uint64
}

// NewEncoder returns an Encoder that writes onto w. When w implements
// http.Flusher (every net/http ResponseWriter that supports
// streaming does), each frame is flushed as it is written so a
// dropped connection doesn't lose more than the last in-flight frame.
func NewEncoder(w io.Writer) *Encoder {
	enc := &Encoder{w: w}
	if f, ok := w.(http.Flusher); ok {
		enc.flusher = f
	}
	return enc
}

// Encode serialises e as one SSE frame and flushes. Returns the
// underlying writer error so callers (the HTTP pump) can bail when
// the client closed the connection.
func (enc *Encoder) Encode(_ context.Context, e Event) error {
	return enc.EncodeWithID(e, strconv.FormatUint(enc.seq.Add(1), 10))
}

// EncodeWithID writes a frame with a stable, caller-supplied SSE cursor.
func (enc *Encoder) EncodeWithID(e Event, id string) error {
	if strings.ContainsAny(id, "\r\n\x00") {
		return fmt.Errorf("invalid SSE event ID")
	}
	body, err := e.Marshal()
	if err != nil {
		return fmt.Errorf("agui encode: marshal: %w", err)
	}

	if id != "" {
		if _, err := fmt.Fprintf(enc.w, "id: %s\n", id); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(enc.w, "event: %s\n", e.EventType()); err != nil {
		return err
	}
	for _, line := range splitData(body) {
		if _, err := fmt.Fprintf(enc.w, "data: %s\n", line); err != nil {
			return err
		}
	}
	if _, err := io.WriteString(enc.w, "\n"); err != nil {
		return err
	}
	enc.flush()
	return nil
}

// EncodeAll runs Encode for every event, stopping at the first
// writer error. Used by the translator's batched output.
func (enc *Encoder) EncodeAll(ctx context.Context, events []Event) error {
	for _, e := range events {
		if err := enc.Encode(ctx, e); err != nil {
			return err
		}
	}
	return nil
}

// Comment writes an SSE comment line. Useful as a keep-alive when
// the broker is quiet for long stretches — some intermediaries drop
// idle SSE connections after 30-60s.
func (enc *Encoder) Comment(text string) error {
	if _, err := fmt.Fprintf(enc.w, ": %s\n\n", text); err != nil {
		return err
	}
	enc.flush()
	return nil
}

func (enc *Encoder) flush() {
	if enc.flusher != nil {
		enc.flusher.Flush()
	}
}

// splitData splits a JSON body on newlines per the SSE spec. Our
// json.Marshal output is single-line, so the slow path almost never
// triggers — but if a future schema embeds a raw string with a
// literal '\n' (e.g. pretty-printed code), this keeps the frame
// valid instead of silently truncating.
func splitData(body []byte) []string {
	s := string(body)
	if !strings.ContainsAny(s, "\n\r") {
		return []string{s}
	}
	// Normalise CRLF then split.
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.Split(s, "\n")
}
