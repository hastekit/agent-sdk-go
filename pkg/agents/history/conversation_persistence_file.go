package history

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bytedance/sonic"
	"go.opentelemetry.io/otel/attribute"
)

const (
	conversationFileExt = ".jsonl"

	recordTypeMessage = "message"
	recordTypeSummary = "summary"
)

// fileRecord is one line of a conversation's JSONL file: either a saved
// message turn or a summary, discriminated by Type.
type fileRecord struct {
	Type    string             `json:"type"`
	Message *fileMessageRecord `json:"message,omitempty"`
	Summary *fileSummaryRecord `json:"summary,omitempty"`
}

// fileMessageRecord stores ThreadID and ConversationID fully resolved (a
// branching save mints a new thread ID), so replay rebuilds the indexes
// without re-running the branching logic.
type fileMessageRecord struct {
	RunID          string         `json:"run_id"`
	PreviousRunID  string         `json:"previous_run_id,omitempty"`
	ThreadID       string         `json:"thread_id"`
	ConversationID string         `json:"conversation_id"`
	Namespace      string         `json:"namespace,omitempty"`
	Messages       []Message      `json:"messages"`
	Meta           map[string]any `json:"meta,omitempty"`
	CreatedAt      time.Time      `json:"created_at"`
}

// fileSummaryRecord is a summary line. The latest record per thread wins on
// replay, matching the in-memory adapter.
type fileSummaryRecord struct {
	ID                  string         `json:"id"`
	ThreadID            string         `json:"thread_id"`
	Namespace           string         `json:"namespace,omitempty"`
	SummaryMessage      Message        `json:"summary_message"`
	LastSummarizedRunID string         `json:"last_summarized_run_id"`
	CreatedAt           time.Time      `json:"created_at"`
	Meta                map[string]any `json:"meta,omitempty"`
}

// FileConversationPersistence is a ConversationPersistenceAdapter that keeps
// conversations on disk as append-only JSONL files, one file per
// conversation (<dir>/<conversationID>.jsonl). All reads are served from
// in-memory indexes rebuilt from the files on startup, so it behaves exactly
// like InMemoryConversationPersistence but survives process restarts.
type FileConversationPersistence struct {
	mu  sync.Mutex
	dir string
	mem *InMemoryConversationPersistence

	// open append handles indexed by conversationID
	files map[string]*os.File
}

// NewFileConversationPersistence replays the JSONL conversation files under
// dir (creating it if needed) to rebuild the conversation state.
func NewFileConversationPersistence(dir string) (*FileConversationPersistence, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("failed to create conversation dir: %w", err)
	}

	p := &FileConversationPersistence{
		dir:   dir,
		mem:   NewInMemoryConversationPersistence(),
		files: make(map[string]*os.File),
	}

	if err := p.replay(); err != nil {
		return nil, err
	}

	return p, nil
}

// NewConversationID generates a unique ID for a conversation
func (p *FileConversationPersistence) NewConversationID(ctx context.Context) string {
	return p.mem.NewConversationID(ctx)
}

// NewRunID generates a unique ID for a run
func (p *FileConversationPersistence) NewRunID(ctx context.Context) string {
	return p.mem.NewRunID(ctx)
}

// LoadMessages retrieves all messages up to and including the previousRunID
func (p *FileConversationPersistence) LoadMessages(ctx context.Context, namespace string, threadID string, previousRunId string) ([]ConversationMessage, error) {
	ctx, span := tracer.Start(ctx, "FileConversationPersistence.LoadMessages")
	defer span.End()

	return p.mem.LoadMessages(ctx, namespace, threadID, previousRunId)
}

// SaveMessages saves messages in memory and appends them to the
// conversation's JSONL file
func (p *FileConversationPersistence) SaveMessages(ctx context.Context, namespace, runId, previousRunId, threadId, conversationId string, messages []Message, meta map[string]any) error {
	ctx, span := tracer.Start(ctx, "FileConversationPersistence.SaveMessages")
	defer span.End()

	span.SetAttributes(
		attribute.String("namespace", namespace),
		attribute.String("previous_run_id", previousRunId),
		attribute.String("thread_id", threadId),
		attribute.String("conversation_id", conversationId),
		attribute.Int("messages_count", len(messages)),
	)

	p.mu.Lock()
	defer p.mu.Unlock()

	if err := p.mem.SaveMessages(ctx, namespace, runId, previousRunId, threadId, conversationId, messages, meta); err != nil {
		return err
	}

	// Read back the stored message: branching resolves the thread and
	// conversation IDs at save time, and the record must carry the
	// resolved values for replay to reconstruct the same state.
	stored := p.mem.getMessage(runId)
	if stored == nil {
		return fmt.Errorf("run %s missing after save", runId)
	}

	f, err := p.fileFor(stored.ConversationID)
	if err != nil {
		return err
	}

	return appendRecord(f, fileRecord{
		Type: recordTypeMessage,
		Message: &fileMessageRecord{
			RunID:          stored.RunID,
			PreviousRunID:  stored.PreviousRunID,
			ThreadID:       stored.ThreadID,
			ConversationID: stored.ConversationID,
			Namespace:      stored.Namespace,
			// Persist this save's increment, not the in-memory readback:
			// a run saved more than once under the same run id merges its
			// increments in memory (see InMemory.SaveMessages), so the
			// readback already holds prior saves. Writing it would double
			// them on replay. Each record carries only its own increment;
			// applyMessageRecord re-appends them in order.
			Messages:  messages,
			Meta:      stored.Meta,
			CreatedAt: stored.CreatedAt,
		},
	})
}

// SaveSummary saves a conversation summary in memory and appends it to the
// conversation's JSONL file
func (p *FileConversationPersistence) SaveSummary(ctx context.Context, namespace string, summary Summary) error {
	ctx, span := tracer.Start(ctx, "FileConversationPersistence.SaveSummary")
	defer span.End()

	p.mu.Lock()
	defer p.mu.Unlock()

	if err := p.mem.SaveSummary(ctx, namespace, summary); err != nil {
		return err
	}

	// Summaries are keyed by thread; place the record in the owning
	// conversation's file, falling back to the thread ID for unknown
	// threads so the record is never dropped.
	conversationID := summary.ThreadID
	if thread := p.mem.getThread(summary.ThreadID); thread != nil {
		conversationID = thread.ConversationID
	}

	f, err := p.fileFor(conversationID)
	if err != nil {
		return err
	}

	return appendRecord(f, fileRecord{
		Type: recordTypeSummary,
		Summary: &fileSummaryRecord{
			ID:                  summary.ID,
			ThreadID:            summary.ThreadID,
			Namespace:           namespace,
			SummaryMessage:      summary.SummaryMessage,
			LastSummarizedRunID: summary.LastSummarizedRunID,
			CreatedAt:           summary.CreatedAt,
			Meta:                summary.Meta,
		},
	})
}

// Close closes all open conversation files
func (p *FileConversationPersistence) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	var errs []error
	for conversationID, f := range p.files {
		if err := f.Close(); err != nil {
			errs = append(errs, fmt.Errorf("conversation %s: %w", conversationID, err))
		}
		delete(p.files, conversationID)
	}

	return errors.Join(errs...)
}

// fileFor returns the append handle for a conversation, opening it lazily
func (p *FileConversationPersistence) fileFor(conversationID string) (*os.File, error) {
	if f, ok := p.files[conversationID]; ok {
		return f, nil
	}

	path := filepath.Join(p.dir, conversationFileName(conversationID))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("failed to open conversation file for %s: %w", conversationID, err)
	}

	p.files[conversationID] = f
	return f, nil
}

// replay rebuilds the in-memory state from every conversation file in the
// directory. Conversations are independent (a thread never spans
// conversations), so per-file line order is the only ordering that matters.
func (p *FileConversationPersistence) replay() error {
	entries, err := os.ReadDir(p.dir)
	if err != nil {
		return fmt.Errorf("failed to read conversation dir: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), conversationFileExt) {
			continue
		}

		if err := p.replayFile(filepath.Join(p.dir, entry.Name())); err != nil {
			return fmt.Errorf("failed to replay %s: %w", entry.Name(), err)
		}
	}

	return nil
}

func (p *FileConversationPersistence) replayFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	return replayLines(f, func(line []byte) error {
		var rec fileRecord
		if err := sonic.Unmarshal(line, &rec); err != nil {
			return err
		}
		return p.applyRecord(&rec)
	})
}

func (p *FileConversationPersistence) applyRecord(rec *fileRecord) error {
	switch rec.Type {
	case recordTypeMessage:
		if rec.Message == nil {
			return fmt.Errorf("message record has no message body")
		}
		p.applyMessageRecord(rec.Message)
	case recordTypeSummary:
		if rec.Summary == nil {
			return fmt.Errorf("summary record has no summary body")
		}
		p.mem.summaries[rec.Summary.ThreadID] = &inMemorySummary{
			ID:                  rec.Summary.ID,
			ThreadID:            rec.Summary.ThreadID,
			Namespace:           rec.Summary.Namespace,
			SummaryMessage:      rec.Summary.SummaryMessage,
			LastSummarizedRunID: rec.Summary.LastSummarizedRunID,
			CreatedAt:           rec.Summary.CreatedAt,
			Meta:                rec.Summary.Meta,
		}
	default:
		return fmt.Errorf("unknown record type %q", rec.Type)
	}

	return nil
}

// applyMessageRecord rebuilds the indexes for one replayed message,
// reconstructing the prefix copy that SaveMessages performs when a save
// branches off the middle of an existing thread.
func (p *FileConversationPersistence) applyMessageRecord(rec *fileMessageRecord) {
	m := p.mem

	// Incremental save replayed: a run saved more than once under the
	// same run id writes one record per increment. Append to the existing
	// run record rather than overwriting it (which would drop the
	// earlier increments) and don't re-index it in the thread — mirrors
	// InMemory.SaveMessages' merge so replay reconstructs the same state.
	if existing, ok := m.messages[rec.RunID]; ok {
		existing.Messages = append(existing.Messages, rec.Messages...)
		if rec.Meta != nil {
			existing.Meta = rec.Meta
		}
		if t := m.threads[existing.ThreadID]; t != nil {
			t.LastRunID = rec.RunID
		}
		return
	}

	thread, exists := m.threads[rec.ThreadID]
	if !exists {
		var prefix []string
		if rec.PreviousRunID != "" {
			if prev, ok := m.messages[rec.PreviousRunID]; ok && prev.ThreadID != rec.ThreadID {
				for _, id := range m.messagesByThread[prev.ThreadID] {
					prefix = append(prefix, id)
					if id == rec.PreviousRunID {
						break
					}
				}
			}
		}

		thread = &inMemoryThread{
			ThreadID:       rec.ThreadID,
			ConversationID: rec.ConversationID,
			OriginRunID:    rec.RunID,
			Namespace:      rec.Namespace,
			CreatedAt:      rec.CreatedAt,
		}
		m.threads[rec.ThreadID] = thread
		m.messagesByThread[rec.ThreadID] = prefix
	}
	thread.LastRunID = rec.RunID

	m.messages[rec.RunID] = &inMemoryMessage{
		RunID:          rec.RunID,
		PreviousRunID:  rec.PreviousRunID,
		ThreadID:       rec.ThreadID,
		ConversationID: rec.ConversationID,
		Namespace:      rec.Namespace,
		Messages:       rec.Messages,
		Meta:           rec.Meta,
		CreatedAt:      rec.CreatedAt,
	}
	m.messagesByThread[rec.ThreadID] = append(m.messagesByThread[rec.ThreadID], rec.RunID)
}

// replayLines invokes apply for each non-empty line of the file. A
// malformed final line (a write interrupted by a crash) is skipped with a
// warning; a malformed line anywhere else is an error.
func replayLines(f *os.File, apply func(line []byte) error) error {
	reader := bufio.NewReader(f)
	lineNo := 0
	for {
		line, readErr := reader.ReadBytes('\n')
		if readErr != nil && readErr != io.EOF {
			return readErr
		}

		atEOF := readErr == io.EOF
		if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 {
			lineNo++
			if err := apply(trimmed); err != nil {
				if !atEOF {
					return fmt.Errorf("line %d: %w", lineNo, err)
				}
				slog.Warn("skipping partial trailing record", "file", f.Name(), "line", lineNo, "error", err)
			}
		}

		if atEOF {
			return nil
		}
	}
}

// conversationFileName maps a conversation ID to a filesystem-safe file
// name. Collisions only co-locate records in one file; replay reads every
// record's own IDs, so correctness is unaffected.
func conversationFileName(conversationID string) string {
	var b strings.Builder
	for _, r := range conversationID {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}

	name := b.String()
	if strings.Trim(name, ".") == "" {
		name = "conversation"
	}

	return name + conversationFileExt
}

func appendRecord(f *os.File, rec fileRecord) error {
	buf, err := sonic.Marshal(rec)
	if err != nil {
		return err
	}

	if _, err := f.Write(append(buf, '\n')); err != nil {
		return err
	}

	return f.Sync()
}
