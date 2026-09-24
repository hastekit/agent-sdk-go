package agui

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
)

var errReplayUnavailable = errors.New("event replay is no longer available; reload thread history and reconnect without Last-Event-ID")

// IDs bind the translated event prefix to one run and one namespace/thread
// channel. The prefix digest also detects trimmed or changed replay sequences.
type eventCursor struct {
	Stream string `json:"s"`
	Run    string `json:"r"`
	Seq    uint64 `json:"n"`
	Hash   string `json:"h"`
}

func (c eventCursor) String() string {
	b, _ := json.Marshal(c)
	return "v1." + base64.RawURLEncoding.EncodeToString(b)
}
func parseEventCursor(raw string) (*eventCursor, error) {
	if raw == "" {
		return nil, nil
	}
	if len(raw) > 2048 || !strings.HasPrefix(raw, "v1.") {
		return nil, fmt.Errorf("invalid Last-Event-ID")
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(raw, "v1."))
	if err != nil {
		return nil, fmt.Errorf("invalid Last-Event-ID")
	}
	var c eventCursor
	if err = json.Unmarshal(b, &c); err != nil || c.Stream == "" || c.Run == "" || c.Seq == 0 {
		return nil, fmt.Errorf("invalid Last-Event-ID")
	}
	digest, err := base64.RawURLEncoding.DecodeString(c.Hash)
	if err != nil || len(digest) != sha256.Size {
		return nil, fmt.Errorf("invalid Last-Event-ID")
	}
	return &c, nil
}

// streamTranslation is shared by POST and rejoin so both yield the same IDs.
// It always processes replayed chunks to reconstruct translator state, even
// when the corresponding events will not be sent to the client.
type streamTranslation struct {
	threadID, streamID string
	translator         *Translator
	runID              string
	seq                uint64
	digest             [sha256.Size]byte
	pending            []*responses.ResponseChunk
}

func (s *streamTranslation) translate(chunk *responses.ResponseChunk) []Event {
	if chunk == nil {
		return nil
	}
	var events []Event
	if s.translator == nil {
		// A complete replay must include run.created. Later lifecycle chunks
		// alone cannot reconstruct open text/tool/step state.
		if chunk.OfRunCreated == nil {
			s.pending = append(s.pending, chunk)
			return nil
		}
		s.runID = runIDOf(chunk)
		display := s.runID
		s.translator = NewTranslator(s.threadID, display)
		events = append(events, s.translator.Start()...)
		events = append(events, &CustomEvent{BaseEvent: baseNow(), Name: CustomNameStreamID, Value: map[string]any{
			"streamId": s.streamID, "runId": display, "threadId": s.threadID,
		}})
		for _, held := range s.pending {
			events = append(events, s.translator.Translate(held)...)
		}
		s.pending = nil
	}
	return append(events, s.translator.Translate(chunk)...)
}
func (s *streamTranslation) eventID(event Event) (string, error) {
	data, err := event.Marshal()
	if err != nil {
		return "", err
	}
	var value map[string]any
	if err = json.Unmarshal(data, &value); err != nil {
		return "", err
	}
	// Rendering timestamps change on replay; all other event fields come
	// from the retained run, including its external correlation ID.
	delete(value, "timestamp")
	canonical, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	s.digest = sha256.Sum256(append(s.digest[:], canonical...))
	s.seq++
	return (eventCursor{Stream: s.streamID, Run: s.runID, Seq: s.seq, Hash: base64.RawURLEncoding.EncodeToString(s.digest[:])}).String(), nil
}

func validateReplay(chunks []*responses.ResponseChunk, threadID, streamID string, cursor *eventCursor) error {
	s := &streamTranslation{threadID: threadID, streamID: streamID}
	for _, chunk := range chunks {
		for _, event := range s.translate(chunk) {
			id, err := s.eventID(event)
			if err != nil {
				return err
			}
			if id == cursor.String() {
				return nil
			}
		}
	}
	return errReplayUnavailable
}

// pumpEvents preserves one canonical ordering for initial streams and rejoins.
func pumpEvents(w http.ResponseWriter, r *http.Request, chunks <-chan *responses.ResponseChunk, threadID, streamID string, cursor *eventCursor, keepalive time.Duration, wait func() error) {
	ctx := r.Context()
	enc := NewEncoder(w)
	state := &streamTranslation{threadID: threadID, streamID: streamID}
	resumed := cursor == nil
	emit := func(events []Event) error {
		for _, event := range events {
			id, err := state.eventID(event)
			if err != nil {
				return err
			}
			if !resumed {
				if id == cursor.String() {
					resumed = true
				}
				continue
			}
			if err := enc.EncodeWithID(event, id); err != nil {
				return err
			}
		}
		return nil
	}
	fail := func(err error) {
		// Transport failures are not part of the replay log: do not advance the
		// client's last-event cursor past the last successfully delivered event.
		translator := state.translator
		if translator == nil {
			translator = NewTranslator(threadID, state.runID)
		}
		for _, event := range translator.Error(err, "replay_error") {
			_ = enc.EncodeWithID(event, "")
		}
	}
	ticker := time.NewTicker(keepalive)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := enc.Comment("keepalive"); err != nil {
				return
			}
		case chunk, ok := <-chunks:
			if !ok {
				if !resumed {
					fail(errReplayUnavailable)
					return
				}
				if wait != nil {
					if err := wait(); err != nil {
						fail(err)
						return
					}
				}
				if state.translator != nil {
					for _, event := range state.translator.Finish() {
						_ = enc.EncodeWithID(event, "")
					}
				}
				return
			}
			if chunk == nil {
				continue
			}
			if chunk.OfRunCreated != nil && ((cursor != nil && runIDOf(chunk) != cursor.Run) || (state.runID != "" && runIDOf(chunk) != state.runID)) {
				fail(errReplayUnavailable)
				return
			}
			events := state.translate(chunk)
			if state.translator == nil && len(state.pending) > 10000 {
				fail(errReplayUnavailable)
				return
			}
			if err := emit(events); err != nil {
				return
			}
			if chunk.OfRunCompleted != nil || chunk.OfRunPaused != nil || chunk.OfRunFailed != nil {
				if !resumed || state.translator == nil {
					fail(errReplayUnavailable)
				}
				return
			}
		}
	}
}
