package streambroker

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// Inject transient read failures and inspect pipeline boundaries without mocking Redis semantics.
type brokerTestHook struct {
	failRead     atomic.Bool
	readFailed   chan struct{}
	pipelines    atomic.Int32
	directWrites atomic.Int32
}

func (h *brokerTestHook) DialHook(next redis.DialHook) redis.DialHook {
	return func(ctx context.Context, network, addr string) (net.Conn, error) { return next(ctx, network, addr) }
}
func (h *brokerTestHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == "xread" && h.failRead.CompareAndSwap(true, false) {
			close(h.readFailed)
			return errors.New("injected read interruption")
		}
		if cmd.Name() == "xadd" || cmd.Name() == "pexpire" {
			h.directWrites.Add(1)
		}
		return next(ctx, cmd)
	}
}
func (h *brokerTestHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error { h.pipelines.Add(1); return next(ctx, cmds) }
}

// Use isolated keys; opt into the integration suite with a disposable Redis or Valkey endpoint.
func testRedisBroker(t *testing.T) (*RedisStreamBroker, *brokerTestHook) {
	t.Helper()
	addr := os.Getenv("HASTEKIT_REDIS_TEST_ADDR")
	if addr == "" {
		t.Skip("set HASTEKIT_REDIS_TEST_ADDR for Redis/Valkey integration tests")
	}
	network := "tcp"
	if strings.HasPrefix(addr, "/") {
		network = "unix"
	}
	client := redis.NewClient(&redis.Options{Addr: addr, Network: network, MaxRetries: -1})
	hook := &brokerTestHook{readFailed: make(chan struct{})}
	client.AddHook(hook)
	broker, err := NewRedisStreamBroker(RedisStreamBrokerOptions{Client: client, Prefix: "stream-test:" + uuid.NewString() + ":", MaxLen: 10000, ActiveTTL: time.Minute})
	require.NoError(t, err)
	broker.blockTime = 20 * time.Millisecond
	t.Cleanup(func() {
		broker.StopRunFeed()
		keys, _ := client.Keys(context.Background(), broker.prefix+"*").Result()
		if len(keys) > 0 {
			_ = client.Del(context.Background(), keys...).Err()
		}
		_ = client.Close()
	})
	return broker, hook
}

// Only initialization sets expiry; subsequent chunks perform a single append.
func TestRedisPublishDoesNotRenewExpiry(t *testing.T) {
	broker, hook := testRedisBroker(t)
	ctx := t.Context()
	require.NoError(t, broker.client.Set(ctx, broker.liveKey("s"), "1", time.Minute).Err())
	// Ignore the client's initial connection metadata pipeline.
	hook.pipelines.Store(0)
	chunk := &responses.ResponseChunk{OfOutputTextDelta: &responses.ChunkOutputText[constants.ChunkTypeOutputTextDelta]{ItemId: "msg", Delta: "private text"}}
	require.NoError(t, broker.Publish(ctx, "s", chunk))
	require.Zero(t, hook.directWrites.Load())
	for _, key := range []string{broker.streamKey("s"), broker.liveKey("s")} {
		ttl, err := broker.client.PTTL(ctx, key).Result()
		require.NoError(t, err)
		require.Greater(t, ttl, 50*time.Second)
	}
	replay, err := broker.Replay(ctx, "s")
	require.NoError(t, err)
	require.Len(t, replay, 1)
	require.Equal(t, "private text", replay[0].OfOutputTextDelta.Delta)

	// Ordinary chunks must not reset either TTL or use a transaction.
	for _, key := range []string{broker.streamKey("s"), broker.liveKey("s")} {
		require.NoError(t, broker.client.PExpire(ctx, key, 3*time.Second).Err())
	}
	hook.pipelines.Store(0)
	hook.directWrites.Store(0)
	require.NoError(t, broker.Publish(ctx, "s", chunk))
	require.Zero(t, hook.directWrites.Load())
	require.Zero(t, hook.pipelines.Load())
	for _, key := range []string{broker.streamKey("s"), broker.liveKey("s")} {
		require.LessOrEqual(t, broker.client.PTTL(ctx, key).Val(), 3*time.Second)
	}

	// Closing must retain replay data and atomically release the active claim.
	require.NoError(t, broker.Close(ctx, "s"))
	require.Zero(t, broker.client.Exists(ctx, broker.liveKey("s")).Val())
	require.Greater(t, broker.client.PTTL(ctx, broker.streamKey("s")).Val(), 9*time.Minute)

}

// A transient blocking-read failure must preserve the subscription and deliver buffered entries once.
func TestRedisSubscriberRetriesReadFailure(t *testing.T) {
	broker, hook := testRedisBroker(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	hook.failRead.Store(true)
	out, err := broker.Subscribe(ctx, "s")
	require.NoError(t, err)
	select {
	case <-hook.readFailed:
	case <-ctx.Done():
		t.Fatal("read never attempted")
	}
	for _, text := range []string{"first", "second"} {
		require.NoError(t, broker.Publish(ctx, "s", &responses.ResponseChunk{OfOutputTextDelta: &responses.ChunkOutputText[constants.ChunkTypeOutputTextDelta]{ItemId: "msg", Delta: text}}))
	}
	require.NoError(t, broker.Close(ctx, "s"))
	var got []string
	for chunk := range out {
		got = append(got, chunk.OfOutputTextDelta.Delta)
	}
	require.NoError(t, ctx.Err())
	require.Equal(t, []string{"first", "second"}, got)
}

// Expected idle reads stay quiet; slow/error logs contain pool statistics but never payloads.
func TestRedisOperationDiagnostics(t *testing.T) {
	client := redis.NewClient(&redis.Options{Addr: "unused:6379"})
	defer client.Close()
	broker := &RedisStreamBroker{client: client, slowOperationThreshold: time.Second}
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(previous)
	broker.observe(t.Context(), "live_read", "stream", time.Now().Add(-5*time.Second), 5*time.Second, redis.Nil)
	require.Empty(t, logs.String())
	broker.observe(t.Context(), "publish", "stream", time.Now().Add(-2*time.Second), 0, nil)
	require.Contains(t, logs.String(), "redis stream operation slow")
	require.Contains(t, logs.String(), "pool_wait_count")
	require.Contains(t, logs.String(), "pool_timeouts")
	logs.Reset()
	broker.observe(t.Context(), "live_read", "stream", time.Now(), 5*time.Second, errors.New("connection reset"))
	require.Contains(t, logs.String(), "redis stream operation failed")
	require.Contains(t, logs.String(), "connection reset")
}

// Agent heartbeats renew both keys without creating response events, including before the first chunk.
func TestRedisHeartbeatRenewsQuietRun(t *testing.T) {
	broker, _ := testRedisBroker(t)
	ctx := t.Context()
	claimed, err := broker.EnqueueOrStart(ctx, "quiet", nil)
	require.NoError(t, err)
	require.True(t, claimed)

	// The initial claim survives quiet startup without creating a transcript.
	require.NoError(t, broker.client.PExpire(ctx, broker.liveKey("quiet"), time.Second).Err())
	require.NoError(t, broker.heartbeat(ctx, "quiet"))
	require.Greater(t, broker.client.PTTL(ctx, broker.liveKey("quiet")).Val(), 50*time.Second)
	require.Zero(t, broker.client.Exists(ctx, broker.streamKey("quiet")).Val())

	// Both expirations refresh during quiet model/tool work, without changing the replay log.
	require.NoError(t, broker.Publish(ctx, "quiet", &responses.ResponseChunk{OfOutputTextDelta: &responses.ChunkOutputText[constants.ChunkTypeOutputTextDelta]{ItemId: "msg", Delta: "text"}}))
	for _, key := range []string{broker.streamKey("quiet"), broker.liveKey("quiet")} {
		require.NoError(t, broker.client.PExpire(ctx, key, time.Second).Err())
	}
	require.NoError(t, broker.heartbeat(ctx, "quiet"))
	for _, key := range []string{broker.streamKey("quiet"), broker.liveKey("quiet")} {
		require.Greater(t, broker.client.PTTL(ctx, key).Val(), 50*time.Second)
	}
	require.EqualValues(t, 1, broker.client.XLen(ctx, broker.streamKey("quiet")).Val())

	// Late heartbeats cannot resurrect a closed run or replace replay retention.
	require.NoError(t, broker.Close(ctx, "quiet"))
	require.NoError(t, broker.heartbeat(ctx, "quiet"))
	require.Zero(t, broker.client.Exists(ctx, broker.liveKey("quiet")).Val())
	require.Greater(t, broker.client.PTTL(ctx, broker.streamKey("quiet")).Val(), 9*time.Minute)
}

// The public lifecycle renews quiet streams until its stop function returns.
func TestRedisStartHeartbeatLifecycle(t *testing.T) {
	broker, _ := testRedisBroker(t)
	broker.heartbeatInterval = 10 * time.Millisecond
	ctx := t.Context()
	claimed, err := broker.EnqueueOrStart(ctx, "quiet", nil)
	require.NoError(t, err)
	require.True(t, claimed)

	// Startup renewal works before any response chunk creates a transcript.
	key := broker.liveKey("quiet")
	require.NoError(t, broker.client.PExpire(ctx, key, time.Second).Err())
	stop := broker.StartHeartbeat(ctx, "quiet")
	defer stop()
	require.Eventually(t, func() bool {
		return broker.client.PTTL(ctx, key).Val() > 50*time.Second
	}, time.Second, time.Millisecond)
	require.Zero(t, broker.client.Exists(ctx, broker.streamKey("quiet")).Val())

	// Cleanup ends renewal, allowing the remaining lease to expire.
	stop()
	require.NoError(t, broker.client.PExpire(ctx, key, 100*time.Millisecond).Err())
	require.Eventually(t, func() bool {
		return broker.client.Exists(ctx, key).Val() == 0
	}, time.Second, 20*time.Millisecond)
}

// Without starting heartbeats, keys expire; publishing does not start renewal.
func TestRedisExpiryWithoutAgentHeartbeat(t *testing.T) {
	broker, _ := testRedisBroker(t)
	ctx := t.Context()
	claimed, err := broker.EnqueueOrStart(ctx, "s", nil)
	require.NoError(t, err)
	require.True(t, claimed)
	require.NoError(t, broker.Publish(ctx, "s", &responses.ResponseChunk{OfOutputTextDelta: &responses.ChunkOutputText[constants.ChunkTypeOutputTextDelta]{ItemId: "msg", Delta: "text"}}))

	// Shorten both keys to demonstrate expiry without waiting for the default TTL.
	for _, key := range []string{broker.streamKey("s"), broker.liveKey("s")} {
		require.NoError(t, broker.client.PExpire(ctx, key, 100*time.Millisecond).Err())
	}
	require.Eventually(t, func() bool {
		return broker.client.Exists(ctx, broker.streamKey("s"), broker.liveKey("s")).Val() == 0
	}, 3*time.Second, 20*time.Millisecond)

	// A late heartbeat cannot recreate an expired run claim or transcript.
	require.NoError(t, broker.heartbeat(ctx, "s"))
	require.Zero(t, broker.client.Exists(ctx, broker.streamKey("s"), broker.liveKey("s")).Val())
}
