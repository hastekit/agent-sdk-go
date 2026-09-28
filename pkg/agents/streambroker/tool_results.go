package streambroker

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/bytedance/sonic"
	"github.com/hastekit/agent-sdk-go/pkg/agents/messages"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/redis/go-redis/v9"
)

// Tool outputs a client sends while its run is live are kept per call for the
// client tool waiting on them (agents.StreamBroker.WaitToolResult), instead of going
// on the run's input queue: the waiting tool is what holds the loop, so the
// loop could never drain a queued output in time. The first output for a call
// wins; a later duplicate is stored nowhere a reader looks. WaitToolResult
// reads them.

// toolResultTTL bounds how long a delivered output waits for its tool.
const toolResultTTL = time.Hour

// closingWait bounds how long a turn waits for a finishing run to release its
// channel before being queued on it anyway.
const closingWait = 10 * time.Second

type toolOutput struct {
	callID  string
	content string
}

// splitToolOutputs separates a turn's function_call_output items from the rest,
// dropping bundles the split leaves empty.
func splitToolOutputs(msgs []messages.Message) (outputs []toolOutput, rest []messages.Message) {
	for _, m := range msgs {
		var kept []responses.InputMessageUnion
		for _, item := range m.Messages {
			if out := item.OfFunctionCallOutput; out != nil && out.CallID != "" {
				outputs = append(outputs, toolOutput{callID: out.CallID, content: toolOutputContent(out)})
				continue
			}
			kept = append(kept, item)
		}
		if len(kept) == len(m.Messages) {
			rest = append(rest, m)
		} else if len(kept) > 0 {
			m.Messages = kept
			rest = append(rest, m)
		}
	}
	return outputs, rest
}

// toolOutputContent is an output as the tool returns it: text as text,
// multipart output as its JSON.
func toolOutputContent(out *responses.FunctionCallOutputMessage) string {
	if out.Output.OfString != nil {
		return *out.Output.OfString
	}
	data, err := sonic.Marshal(out.Output.OfList)
	if err != nil {
		return ""
	}
	return string(data)
}

// isRunEnd reports whether a chunk is the last thing a run publishes about
// itself. Its state is saved by then, so the channel is only closing.
func isRunEnd(chunk *responses.ResponseChunk) bool {
	return chunk != nil && (chunk.OfRunCompleted != nil || chunk.OfRunPaused != nil || chunk.OfRunFailed != nil)
}

// toolResultStore keeps delivered outputs in memory until a waiting tool takes
// them or they expire.
type toolResultStore struct {
	mu      sync.Mutex
	entries map[string]*toolResult
}

type toolResult struct {
	result    string
	delivered bool
	ready     chan struct{}
	expires   time.Time
}

// entry returns the record for key, creating it and pruning expired ones. Callers hold mu.
func (s *toolResultStore) entry(key string, now time.Time) *toolResult {
	if s.entries == nil {
		s.entries = map[string]*toolResult{}
	}
	for k, e := range s.entries {
		if now.After(e.expires) {
			delete(s.entries, k)
		}
	}
	e, ok := s.entries[key]
	if !ok {
		e = &toolResult{ready: make(chan struct{}), expires: now.Add(toolResultTTL)}
		s.entries[key] = e
	}
	return e
}

func (s *toolResultStore) deliver(key, result string) {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entry(key, now)
	if e.delivered {
		return
	}
	e.result, e.delivered, e.expires = result, true, now.Add(toolResultTTL)
	close(e.ready)
}

func (s *toolResultStore) await(ctx context.Context, key string) (string, error) {
	s.mu.Lock()
	e := s.entry(key, time.Now())
	s.mu.Unlock()
	// Checked first so an expired context still reads a result already there.
	select {
	case <-e.ready:
		return e.result, nil
	default:
	}
	select {
	case <-e.ready:
		return e.result, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func toolResultKey(channel, callID string) string {
	return channel + "\x00" + callID
}

// waitWithTimeout runs await with timeout as its deadline, reporting a result
// that did not arrive in time as not found rather than as an error. A zero
// timeout still takes a result already there: await checks before waiting.
func waitWithTimeout(ctx context.Context, timeout time.Duration, await func(context.Context) (string, error)) (string, bool, error) {
	waitCtx, cancel := context.WithTimeout(ctx, max(timeout, 0))
	defer cancel()
	result, err := await(waitCtx)
	switch {
	case err == nil:
		return result, true, nil
	case ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded):
		return "", false, nil
	default:
		return "", false, err
	}
}

// WaitToolResult implements agents.StreamBroker.
func (b *MemoryStreamBroker) WaitToolResult(ctx context.Context, channel, callID string, timeout time.Duration) (string, bool, error) {
	return waitWithTimeout(ctx, timeout, func(ctx context.Context) (string, error) {
		return b.toolResults.await(ctx, toolResultKey(channel, callID))
	})
}

func (b *RedisStreamBroker) toolResultKey(channel, callID string) string {
	return b.prefix + "tool-result:" + channel + ":" + callID
}

func (b *RedisStreamBroker) toolResultNotifyChannel(channel, callID string) string {
	return b.prefix + "tool-result-notify:" + channel + ":" + callID
}

// toolResultRecheck bounds how long a waiter relies on pub/sub alone.
const toolResultRecheck = 5 * time.Second

// WaitToolResult implements agents.StreamBroker. The output stays stored, so a
// retried activity or step reads the same answer.
func (b *RedisStreamBroker) WaitToolResult(ctx context.Context, channel, callID string, timeout time.Duration) (string, bool, error) {
	return waitWithTimeout(ctx, timeout, func(ctx context.Context) (string, error) {
		return b.awaitToolResult(ctx, channel, callID)
	})
}

func (b *RedisStreamBroker) awaitToolResult(ctx context.Context, channel, callID string) (string, error) {
	key := b.toolResultKey(channel, callID)
	// A result already there is returned even to a context with no time left.
	if result, err := b.client.Get(context.WithoutCancel(ctx), key).Result(); err == nil {
		return result, nil
	} else if !errors.Is(err, redis.Nil) {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}

	// Subscribe before reading again, so a delivery between the two is not missed.
	sub := b.client.Subscribe(ctx, b.toolResultNotifyChannel(channel, callID))
	defer sub.Close()
	if _, err := sub.Receive(ctx); err != nil {
		return "", err
	}
	notifications := sub.Channel()
	ticker := time.NewTicker(toolResultRecheck)
	defer ticker.Stop()
	for {
		result, err := b.client.Get(ctx, key).Result()
		if err == nil {
			return result, nil
		}
		if !errors.Is(err, redis.Nil) {
			return "", err
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-notifications:
		case <-ticker.C:
		}
	}
}
