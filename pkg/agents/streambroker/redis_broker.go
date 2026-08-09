package streambroker

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/bytedance/sonic"
	"github.com/hastekit/hastekit-sdk-go/pkg/agents/messages"
	"github.com/hastekit/hastekit-sdk-go/pkg/gateway/llm/responses"
	"github.com/redis/go-redis/v9"
)

// RedisStreamBroker implements StreamBroker using Redis Streams.
// Each channel (typically a stream id) maps to one Redis Stream that
// persists every chunk emitted for it. Subscribers always receive the
// full transcript from the first event and then tail any remaining
// events until Close is called. This makes streams rejoinable: a
// client that drops mid-stream can reconnect to the same channel and
// receive the complete transcript.
//
// Redis Streams give us:
//   - Durable event log with MAXLEN cap to bound memory
//   - Multiple independent readers per channel (no coordination needed)
//   - TTL-based automatic cleanup after a stream terminates
type RedisStreamBroker struct {
	client       *redis.Client
	prefix       string
	activeTTL    time.Duration
	replayTTL    time.Duration
	maxLen       int64
	readCount    int64
	blockTime    time.Duration
	stopPollTime time.Duration
}

// RedisStreamBrokerOptions configures the Redis stream broker.
type RedisStreamBrokerOptions struct {
	// Addr is the Redis server address (e.g., "localhost:6379").
	Addr string

	// Password is the Redis password (optional).
	Password string

	// DB is the Redis database number (default 0).
	DB int

	// Prefix is prepended to all channel names (default "uno:stream:").
	// This allows multiple applications to share the same Redis instance.
	Prefix string

	// Client is an existing Redis client to use instead of creating a new one.
	// If provided, Addr/Password/DB are ignored.
	Client *redis.Client

	// ActiveTTL bounds the lifetime of a channel's stream while it is
	// still active. Default 30 minutes.
	ActiveTTL time.Duration

	// ReplayTTL is the rejoin window applied after Close. Default 10 minutes.
	ReplayTTL time.Duration

	// MaxLen caps the approximate number of entries retained per stream
	// (XADD MAXLEN ~). Default 2000.
	MaxLen int64

	// StopPollInterval is how often WatchStop re-reads the stop flag,
	// bounding how long a stop takes to interrupt a running tool.
	// Default 500ms, and only paid while a tool is executing.
	StopPollInterval time.Duration
}

const (
	defaultActiveTTL = 30 * time.Minute
	defaultReplayTTL = 10 * time.Minute
	defaultMaxLen    = int64(2000)
	defaultReadCount = int64(500)
	defaultBlock     = 5 * time.Second
	defaultStopPoll  = 500 * time.Millisecond

	// streamEndType is the sentinel event type written by Close so
	// Subscribe loops can terminate without relying on a status key.
	streamEndType = "__stream_end"
)

// NewRedisStreamBroker creates a new Redis-backed stream broker.
func NewRedisStreamBroker(opts RedisStreamBrokerOptions) (*RedisStreamBroker, error) {
	var client *redis.Client

	if opts.Client != nil {
		client = opts.Client
	} else {
		client = redis.NewClient(&redis.Options{
			Addr:     opts.Addr,
			Password: opts.Password,
			DB:       opts.DB,
		})
	}

	// Test connection
	if err := client.Ping(context.Background()).Err(); err != nil {
		return nil, fmt.Errorf("failed to connect to Redis: %w", err)
	}

	prefix := opts.Prefix
	if prefix == "" {
		prefix = "uno:stream:"
	}

	activeTTL := opts.ActiveTTL
	if activeTTL <= 0 {
		activeTTL = defaultActiveTTL
	}
	replayTTL := opts.ReplayTTL
	if replayTTL <= 0 {
		replayTTL = defaultReplayTTL
	}
	maxLen := opts.MaxLen
	if maxLen <= 0 {
		maxLen = defaultMaxLen
	}
	stopPoll := opts.StopPollInterval
	if stopPoll <= 0 {
		stopPoll = defaultStopPoll
	}

	return &RedisStreamBroker{
		client:       client,
		prefix:       prefix,
		activeTTL:    activeTTL,
		replayTTL:    replayTTL,
		maxLen:       maxLen,
		readCount:    defaultReadCount,
		blockTime:    defaultBlock,
		stopPollTime: stopPoll,
	}, nil
}

// streamKey returns the Redis Stream key for a channel.
func (b *RedisStreamBroker) streamKey(channel string) string {
	return b.prefix + channel
}

// stopKey returns the Redis key holding the stop flag for a channel.
func (b *RedisStreamBroker) stopKey(channel string) string {
	return b.prefix + "stop:" + channel
}

// queueKey returns the Redis list key holding queued input messages
// for a channel.
func (b *RedisStreamBroker) queueKey(channel string) string {
	return b.prefix + "queue:" + channel
}

// liveKey holds the run-claim flag for a channel — set while a run is in
// flight on a (deterministic) stream id and released by Close. Its TTL is
// a crash backstop and is refreshed on every Publish.
func (b *RedisStreamBroker) liveKey(channel string) string {
	return b.prefix + "live:" + channel
}

// Publish appends a chunk to the channel's Redis Stream. The stream
// key has its active TTL refreshed on first write so a long-lived
// stream doesn't silently expire.
func (b *RedisStreamBroker) Publish(ctx context.Context, channel string, chunk *responses.ResponseChunk) error {
	data, err := sonic.Marshal(chunk)
	if err != nil {
		return fmt.Errorf("failed to serialize chunk: %w", err)
	}

	key := b.streamKey(channel)
	id, err := b.client.XAdd(ctx, &redis.XAddArgs{
		Stream: key,
		MaxLen: b.maxLen,
		Approx: true,
		Values: map[string]any{
			"type":    chunk.ChunkType(),
			"payload": data,
		},
	}).Result()
	if err != nil {
		return fmt.Errorf("failed to publish chunk: %w", err)
	}

	// Refresh TTL on first write (id ends in "-0" for the very first
	// entry of a millisecond — cheap best-effort; re-EXPIRE is idempotent).
	// The run-claim flag rides the same liveness so it survives long runs
	// and only expires after the run goes silent (crash backstop).
	if id != "" {
		_ = b.client.Expire(ctx, key, b.activeTTL).Err()
		_ = b.client.Expire(ctx, b.liveKey(channel), b.activeTTL).Err()
	}

	return nil
}

// Subscribe returns a channel that delivers every chunk of `channel`
// from the first event onward. It first drains the existing stream via
// XRANGE and then tails live entries with XREAD BLOCK. The output
// channel closes when Close has been called (the __stream_end sentinel
// is seen) or the context is cancelled.
//
// Multiple subscribers may be active for the same channel concurrently;
// each receives the full transcript independently.
func (b *RedisStreamBroker) Subscribe(ctx context.Context, channel string) (<-chan *responses.ResponseChunk, error) {
	out := make(chan *responses.ResponseChunk, 100)

	go func() {
		defer close(out)

		key := b.streamKey(channel)

		// Phase 1 — replay everything currently in the stream using
		// paginated XRANGE. lastID tracks the id of the last entry we
		// emitted so Phase 2's XREAD picks up from the right place.
		lastID := "0"
		cursor := "-"
		for {
			entries, err := b.client.XRangeN(ctx, key, cursor, "+", b.readCount).Result()
			if err != nil || len(entries) == 0 {
				break
			}
			endSeen := false
			for _, entry := range entries {
				lastID = entry.ID
				chunk, isEnd, ok := decodeEntry(entry)
				if isEnd {
					endSeen = true
					break
				}
				if !ok {
					continue
				}
				select {
				case out <- chunk:
				case <-ctx.Done():
					return
				}
			}
			if endSeen {
				return
			}
			// XRANGE's start is inclusive, so bump past the last id.
			cursor = incrementID(lastID)
		}

		// Phase 2 — live tail. XREAD is exclusive of the passed id.
		for {
			if ctx.Err() != nil {
				return
			}
			res, err := b.client.XRead(ctx, &redis.XReadArgs{
				Streams: []string{key, lastID},
				Count:   b.readCount,
				Block:   b.blockTime,
			}).Result()
			if err == redis.Nil {
				continue
			}
			if err != nil {
				return
			}
			for _, stream := range res {
				for _, entry := range stream.Messages {
					lastID = entry.ID
					chunk, isEnd, ok := decodeEntry(entry)
					if isEnd {
						return
					}
					if !ok {
						continue
					}
					select {
					case out <- chunk:
					case <-ctx.Done():
						return
					}
				}
			}
		}
	}()

	return out, nil
}

// Close writes a stream-end sentinel so active subscribers terminate
// their XREAD BLOCK loops, then shortens the stream's TTL to the
// replay window. Idempotent.
func (b *RedisStreamBroker) Close(ctx context.Context, channel string) error {
	key := b.streamKey(channel)

	if _, err := b.client.XAdd(ctx, &redis.XAddArgs{
		Stream: key,
		MaxLen: b.maxLen,
		Approx: true,
		Values: map[string]any{
			"type":    streamEndType,
			"payload": "{}",
		},
	}).Result(); err != nil {
		return fmt.Errorf("failed to write stream-end sentinel: %w", err)
	}

	_ = b.client.Expire(ctx, key, b.replayTTL).Err()
	// Release the run claim so the next turn for this channel starts fresh.
	_ = b.client.Del(ctx, b.liveKey(channel)).Err()
	return nil
}

// Stop records a stop request for the channel by setting a flag key
// with the active TTL. The agent loop polls this via IsStopped.
func (b *RedisStreamBroker) Stop(ctx context.Context, channel string) error {
	if err := b.client.Set(ctx, b.stopKey(channel), "1", b.activeTTL).Err(); err != nil {
		return fmt.Errorf("failed to set stop flag: %w", err)
	}
	return nil
}

// IsStopped reports whether Stop has been called for the channel.
func (b *RedisStreamBroker) IsStopped(ctx context.Context, channel string) (bool, error) {
	n, err := b.client.Exists(ctx, b.stopKey(channel)).Result()
	if err != nil {
		return false, fmt.Errorf("failed to read stop flag: %w", err)
	}
	return n > 0, nil
}

// WatchStop implements StopWatcher. The flag is usually set by another
// process, so polling is the only way to learn of it promptly — and only
// for the lifetime of the watch, i.e. while a tool call is in flight.
// Transient Redis errors are ignored so a blip doesn't drop the watch.
func (b *RedisStreamBroker) WatchStop(ctx context.Context, channel string) (<-chan struct{}, func()) {
	out := make(chan struct{})
	watchCtx, cancel := context.WithCancel(ctx)

	go func() {
		ticker := time.NewTicker(b.stopPollTime)
		defer ticker.Stop()

		for {
			select {
			case <-watchCtx.Done():
				return
			case <-ticker.C:
				stopped, err := b.IsStopped(watchCtx, channel)
				if err != nil || !stopped {
					continue
				}
				close(out)
				return
			}
		}
	}()

	return out, cancel
}

// EnqueueOrStart implements RunClaimBroker. The claim is an atomic SETNX
// on liveKey: the winner resets the reused channel and starts a fresh run;
// everyone else appends to the run's queue.
func (b *RedisStreamBroker) EnqueueOrStart(ctx context.Context, channel string, msgs []messages.Message) (bool, error) {
	claimed, err := b.client.SetNX(ctx, b.liveKey(channel), "1", b.activeTTL).Result()
	if err != nil {
		return false, fmt.Errorf("failed to claim run: %w", err)
	}

	if !claimed {
		key := b.queueKey(channel)
		pipe := b.client.TxPipeline()
		for _, m := range msgs {
			data, err := sonic.Marshal(m)
			if err != nil {
				return false, fmt.Errorf("failed to serialize message: %w", err)
			}
			pipe.RPush(ctx, key, data)
		}
		pipe.Expire(ctx, key, b.activeTTL)
		if _, err := pipe.Exec(ctx); err != nil {
			return false, fmt.Errorf("failed to enqueue message: %w", err)
		}
		return false, nil
	}

	// Won the claim — clear any stale transcript / queue / stop so a reused
	// channel never replays a previous turn into the fresh run.
	b.client.Del(ctx, b.streamKey(channel), b.queueKey(channel), b.stopKey(channel))
	return true, nil
}

// EnqueueMessage appends a JSON-encoded message to the channel's queue
// list. The list TTL is refreshed on each push.
func (b *RedisStreamBroker) EnqueueMessage(ctx context.Context, channel string, msg messages.Message) error {
	data, err := sonic.Marshal(msg)
	if err != nil {
		return fmt.Errorf("failed to serialize message: %w", err)
	}
	key := b.queueKey(channel)
	pipe := b.client.TxPipeline()
	pipe.RPush(ctx, key, data)
	pipe.Expire(ctx, key, b.activeTTL)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("failed to enqueue message: %w", err)
	}
	return nil
}

// DrainMessages atomically returns and clears all queued messages.
// LRANGE+DEL inside MULTI/EXEC ensures concurrent drains never see
// partial state.
func (b *RedisStreamBroker) DrainMessages(ctx context.Context, channel string) ([]messages.Message, error) {
	key := b.queueKey(channel)
	pipe := b.client.TxPipeline()
	rangeCmd := pipe.LRange(ctx, key, 0, -1)
	pipe.Del(ctx, key)
	if _, err := pipe.Exec(ctx); err != nil {
		return nil, fmt.Errorf("failed to drain messages: %w", err)
	}

	raw, err := rangeCmd.Result()
	if err != nil {
		return nil, fmt.Errorf("failed to read drained messages: %w", err)
	}

	out := make([]messages.Message, 0, len(raw))
	for _, s := range raw {
		var msg messages.Message
		if err := sonic.Unmarshal([]byte(s), &msg); err != nil {
			// Skip malformed entries rather than failing the whole drain.
			continue
		}
		out = append(out, msg)
	}
	return out, nil
}

// IsActive reports whether the channel has an in-flight run. The
// stream key existing isn't sufficient — Close shrinks its TTL but
// the key lingers in the replay window. We additionally check whether
// the stream-end sentinel has been written.
func (b *RedisStreamBroker) IsActive(ctx context.Context, channel string) (bool, error) {
	key := b.streamKey(channel)
	n, err := b.client.Exists(ctx, key).Result()
	if err != nil {
		return false, fmt.Errorf("failed to check stream existence: %w", err)
	}
	if n == 0 {
		return false, nil
	}

	// Latest entry — if it's the stream-end sentinel, the run terminated.
	entries, err := b.client.XRevRangeN(ctx, key, "+", "-", 1).Result()
	if err != nil {
		return false, fmt.Errorf("failed to read latest entry: %w", err)
	}
	if len(entries) == 0 {
		// Key exists but no entries — treat as not active.
		return false, nil
	}
	if t, _ := entries[0].Values["type"].(string); t == streamEndType {
		return false, nil
	}
	return true, nil
}

// GetClient returns the underlying Redis client.
func (b *RedisStreamBroker) GetClient() *redis.Client {
	return b.client
}

// decodeEntry parses a Redis stream entry into a ResponseChunk. The
// second return is true when the entry is the stream-end sentinel. The
// third return is false when the entry's payload is malformed.
func decodeEntry(entry redis.XMessage) (*responses.ResponseChunk, bool, bool) {
	evType, _ := entry.Values["type"].(string)
	if evType == streamEndType {
		return nil, true, false
	}
	payload, _ := entry.Values["payload"].(string)
	var chunk responses.ResponseChunk
	if err := sonic.Unmarshal([]byte(payload), &chunk); err != nil {
		return nil, false, false
	}
	return &chunk, false, true
}

// incrementID returns the smallest Redis stream id strictly greater
// than id. Redis ids are "<ms>-<seq>"; incrementing the seq suffices.
func incrementID(id string) string {
	for i := len(id) - 1; i >= 0; i-- {
		if id[i] == '-' {
			seq, err := strconv.ParseUint(id[i+1:], 10, 64)
			if err != nil {
				return id
			}
			return id[:i+1] + strconv.FormatUint(seq+1, 10)
		}
	}
	return id
}
