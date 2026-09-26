# Redis stream diagnostics

`RedisStreamBroker` appends chunks without renewing their TTLs. The first append
atomically sets the stream's initial expiry. The executing agent starts a separate
renewal loop through `StreamBroker.StartHeartbeat`; Redis renews the existing
stream and live-claim keys internally. Heartbeats do not enter the
replay log or reach UI subscribers. Renewal runs only during execution.

`HeartbeatInterval` defaults to the smaller of one minute and `ActiveTTL / 3` and
must be less than `ActiveTTL` (30 minutes by default). Quiet model calls and tools
continue to receive heartbeats. When execution stops, emission stops; a crashed
worker's keys expire after the last successful renewal. An outage lasting longer
than the TTL can still lose the stream or claim; heartbeats do not recreate them.
Direct broker publishers outside the agent must also start and stop heartbeats.

`ExecuteLocal` calls `StreamBroker.StartHeartbeat(ctx, streamID)` at the beginning
and invokes the returned idempotent stop function before closing the stream.
There is no separate agent heartbeat option, consumer interface, or runtime-interface method.

- Memory brokers return a no-op because their streams do not expire.
- Redis brokers start an execution-scoped Go emitter. Publishing never starts it.
- Restate broker proxies delegate to the real broker without journaling liveness.
- Temporal broker proxies schedule one long-lived heartbeat activity, passing only
  the explicit stream ID. The activity calls the real broker's `StartHeartbeat`
  and stops it on cancellation. Renewal continues while the workflow is idle
  between model/tool calls. Separate Temporal activity heartbeats handle worker
  failure detection and cancellation delivery.

At execution end, Temporal requests activity cancellation and waits for completion
with `WaitForCancellation` before the stream is closed. Cleanup uses a disconnected
workflow context so cancellation of the workflow does not skip this join. The
activity has a 24-hour attempt limit, a 5-second Temporal heartbeat timeout, and
retry backoff capped at 5 seconds. Broker renewal uses `HeartbeatInterval`;
Temporal liveness is recorded every second, subject to SDK heartbeat throttling.

The activity's lifecycle is durable; individual broker renewals are not replay
messages or workflow events. This uses one activity execution slot per active
stream with a configured broker, without inspecting its retention capabilities.
Background task streams get their own activity,
stopped before their stream closes. The registered agent activity map includes
the heartbeat implementation; no worker interceptor or context propagator is
needed. Versioning preserves older workflow histories without adding new activity
commands to their replay; new executions use the dedicated activity.

A queued heartbeat activity or worker outage can still delay renewal beyond the
TTL. Heartbeats cannot recover keys that have already expired. An activity retry
resumes renewing existing keys after the worker recovers.

Closing pipelines the end marker, replay TTL, and claim release. Heartbeat renewal
checks the end marker atomically so a late heartbeat cannot extend a completed
stream's replay window. A subscriber retries failed reads from its last cursor
until its context is cancelled rather than treating a Redis interruption as EOF.

The broker uses the application's default `slog` logger. No chunk payloads,
prompts, or credentials are included in its diagnostic fields.

- `redis stream operation slow`: operations exceeding `SlowOperationThreshold`
  (default 250 ms). Blocking reads allow their configured block duration plus
  this threshold; normal idle read timeouts do not generate warnings.
- `redis stream operation failed`: Redis errors, including expiry-update failures.
- `agent stream heartbeat failed`: execution could not report liveness; the next
  scheduled heartbeat retries.
- `agent stream publish failed`: the agent could not publish a chunk through its
  broker. Publishing failures remain non-fatal to model execution.

Fields include `operation`, `stream_id`, `duration`, `expected_wait`,
`pool_total_conns`, `pool_idle_conns`, `pool_timeouts`, `pool_wait_count`, and
`pool_wait_duration`. Pool wait and timeout fields are cumulative counters for
this Redis client, not measurements specific to that operation. Use their deltas
when correlating an incident. `broker.PoolStats()` exposes the same counters to
application metrics collectors without requiring a slow operation first.

`subscriber_delivery` identifies a slow downstream consumer separately from
`publish`, `heartbeat`, `live_read`, `replay_read`, and `replay_snapshot`. For a deployed stall,
correlate these logs with application/Valkey pod CPU throttling, Valkey latency,
and ingress buffering. Absence of a warning does not prove that the LLM or ingress
was streaming continuously.

Run integration tests against a disposable Redis or Valkey server:

```sh
HASTEKIT_REDIS_TEST_ADDR=127.0.0.1:6379 go test -race ./pkg/agents/streambroker
```

The test address may also be a Unix socket path. Tests use unique key prefixes
and clean up their own keys.

Both brokers retain up to 50,000 events per stream by default (approximately for
Redis). Redis callers can override this with `RedisStreamBrokerOptions.MaxLen`.
This is an event-count limit, not a memory budget: tool results and other payloads
can be much larger than text deltas. Runs exceeding the limit still lose their
oldest replay events. Redis expires completed streams after `ReplayTTL`; the
in-memory broker currently has no automatic expiry for completed transcripts.
