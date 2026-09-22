package routines

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

type RedisSchedulerConfig struct {
	SchedulerConfig
	// Prefix identifies one definition store. Use a distinct prefix per store.
	// The default hash tag keeps all keys in one Redis Cluster slot.
	Prefix        string
	LeaseDuration time.Duration // default 30s; minimum 100ms
}

// RedisScheduler persists execution state and due times in Redis. A renewable
// fenced lease permits one active owner of a prefix. The client is caller-owned.
// Use persistent, non-evicting Redis for restart recovery; JSONL holds no backup
// of this execution state.
type RedisScheduler struct {
	engine  schedulerEngine
	backend *redisState
	lease   time.Duration
	running atomic.Bool
}

func NewRedisScheduler(service *Service, client redis.UniversalClient, config RedisSchedulerConfig) *RedisScheduler {
	if config.Prefix == "" {
		config.Prefix = "{hastekit:routines}"
	}
	if config.LeaseDuration <= 0 {
		config.LeaseDuration = 30 * time.Second
	}
	if config.LeaseDuration < 100*time.Millisecond {
		config.LeaseDuration = 100 * time.Millisecond
	}
	backend := &redisState{client: client, prefix: config.Prefix}
	return &RedisScheduler{engine: schedulerEngine{service: service, config: config.SchedulerConfig.defaults(), backend: backend}, backend: backend, lease: config.LeaseDuration}
}
func (s *RedisScheduler) Status(ctx context.Context, ns, id string) (RoutineState, error) {
	if s.backend.client == nil {
		return RoutineState{}, errors.New("Redis client is required")
	}
	raw, err := s.backend.client.HGet(ctx, s.backend.prefix+":state", key(namespace(ns), id)).Result()
	if errors.Is(err, redis.Nil) {
		return RoutineState{}, ErrNotFound
	}
	if err != nil {
		return RoutineState{}, err
	}
	var r scheduledRoutine
	if err = json.Unmarshal([]byte(raw), &r); err != nil {
		return RoutineState{}, err
	}
	return r.State, nil
}
func (s *RedisScheduler) Run(ctx context.Context) error {
	if s.backend.client == nil {
		return errors.New("Redis client is required")
	}
	if !s.running.CompareAndSwap(false, true) {
		return errors.New("scheduler already running")
	}
	defer s.running.Store(false)
	if !s.engine.service.running.CompareAndSwap(false, true) {
		return errors.New("service already has a running scheduler")
	}
	defer s.engine.service.running.Store(false)
	s.backend.owner = uuid.NewString()
	ok, err := s.backend.client.SetNX(ctx, s.backend.prefix+":owner", s.backend.owner, s.lease).Result()
	if err != nil {
		return err
	}
	if !ok {
		return ErrSchedulerOwned
	}
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = redisRelease.Run(releaseCtx, s.backend.client, []string{s.backend.prefix + ":owner"}, s.backend.owner).Result()
	}()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	leaseDone := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(s.lease / 3)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				leaseDone <- nil
				return
			case <-ticker.C:
				renewCtx, stop := context.WithTimeout(ctx, s.lease/3)
				n, err := redisRenew.Run(renewCtx, s.backend.client, []string{s.backend.prefix + ":owner"}, s.backend.owner, s.lease.Milliseconds()).Int()
				stop()
				if err != nil || n != 1 {
					if err == nil {
						err = ErrSchedulerLeaseLost
					}
					cancel()
					leaseDone <- err
					return
				}
			}
		}
	}()
	err = s.engine.run(ctx)
	cancel()
	if leaseErr := <-leaseDone; leaseErr != nil {
		return fmt.Errorf("renew scheduler lease: %w", leaseErr)
	}
	return err
}

type redisState struct {
	client        redis.UniversalClient
	prefix, owner string
}

func (s *redisState) keys() []string {
	return []string{s.prefix + ":owner", s.prefix + ":state", s.prefix + ":due"}
}
func (s *redisState) Load(ctx context.Context) (map[string]scheduledRoutine, error) {
	raw, err := s.client.HGetAll(ctx, s.prefix+":state").Result()
	if err != nil {
		return nil, err
	}
	out := map[string]scheduledRoutine{}
	for k, v := range raw {
		var r scheduledRoutine
		if err := json.Unmarshal([]byte(v), &r); err != nil {
			return nil, fmt.Errorf("decode Redis routine state: %w", err)
		}
		out[k] = r
	}
	return out, nil
}
func (s *redisState) Save(ctx context.Context, k string, r scheduledRoutine) error {
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	score := ""
	if at := r.dueAt(); !at.IsZero() {
		score = strconv.FormatInt(at.UnixMilli(), 10)
	}
	return s.write(ctx, k, string(raw), score)
}
func (s *redisState) Delete(ctx context.Context, k string) error { return s.write(ctx, k, "", "") }
func (s *redisState) write(ctx context.Context, k, raw, score string) error {
	n, err := redisWrite.Run(ctx, s.client, s.keys(), s.owner, k, raw, score).Int()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrSchedulerLeaseLost
	}
	return nil
}
func (s *redisState) Due(ctx context.Context, now time.Time) ([]string, error) {
	// Verify ownership before selecting work as well as when persisting a claim.
	owner, err := s.client.Get(ctx, s.prefix+":owner").Result()
	if errors.Is(err, redis.Nil) {
		return nil, ErrSchedulerLeaseLost
	}
	if err != nil {
		return nil, err
	}
	if owner != s.owner {
		return nil, ErrSchedulerLeaseLost
	}
	return s.client.ZRangeByScore(ctx, s.prefix+":due", &redis.ZRangeBy{Min: "-inf", Max: strconv.FormatInt(now.UnixMilli(), 10)}).Result()
}

var redisWrite = redis.NewScript(strings.TrimSpace(`
if redis.call('GET',KEYS[1]) ~= ARGV[1] then return 0 end
if ARGV[3] == '' then redis.call('HDEL',KEYS[2],ARGV[2]) else redis.call('HSET',KEYS[2],ARGV[2],ARGV[3]) end
if ARGV[4] == '' then redis.call('ZREM',KEYS[3],ARGV[2]) else redis.call('ZADD',KEYS[3],ARGV[4],ARGV[2]) end
return 1
`))
var redisRenew = redis.NewScript(`if redis.call('GET',KEYS[1]) == ARGV[1] then return redis.call('PEXPIRE',KEYS[1],ARGV[2]) end return 0`)
var redisRelease = redis.NewScript(`if redis.call('GET',KEYS[1]) == ARGV[1] then return redis.call('DEL',KEYS[1]) end return 0`)
var _ Scheduler = (*RedisScheduler)(nil)

func (s *RedisScheduler) RunNow(ctx context.Context, ns, id string) (Run, error) {
	return s.engine.runNow(ctx, ns, id)
}
