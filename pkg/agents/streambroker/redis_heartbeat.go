package streambroker

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

// appendStream sets expiry only on creation; normal chunks do not renew either TTL.
var appendStream = redis.NewScript(`
local exists = redis.call('EXISTS', KEYS[1])
local id = redis.call('XADD', KEYS[1], 'MAXLEN', '~', ARGV[2], '*', 'type', ARGV[3], 'payload', ARGV[4])
if exists == 0 then
    redis.call('PEXPIRE', KEYS[1], ARGV[1])
end
return id
`)

// renewStream is atomic with Close, so a late heartbeat cannot replace replay retention.
var renewStream = redis.NewScript(`
local last = redis.call('XREVRANGE', KEYS[1], '+', '-', 'COUNT', 1)
if #last > 0 then
    local fields = last[1][2]
    for i = 1, #fields, 2 do
        if fields[i] == 'type' and fields[i + 1] == ARGV[2] then
            return 0
        end
    end
end
redis.call('PEXPIRE', KEYS[1], ARGV[1])
redis.call('PEXPIRE', KEYS[2], ARGV[1])
return 1
`)

// heartbeat renews existing keys when the agent reports that its execution is alive.
// It never creates keys, appends replay events, or starts background work.
func (b *RedisStreamBroker) heartbeat(ctx context.Context, channel string) error {
	started := time.Now()
	err := renewStream.Run(ctx, b.client, []string{b.streamKey(channel), b.liveKey(channel)},
		b.activeTTL.Milliseconds(), streamEndType).Err()
	b.observe(ctx, "heartbeat", channel, started, 0, err)
	return err
}

// StartHeartbeat renews retention only for the lifetime explicitly owned by the executing agent.
func (b *RedisStreamBroker) StartHeartbeat(ctx context.Context, channel string) func() {
	return startHeartbeat(ctx, channel, b.heartbeatInterval, b.heartbeat)
}
