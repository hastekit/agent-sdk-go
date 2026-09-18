package streambroker

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/bytedance/sonic"
)

// runFeedChannel is the pub/sub channel a namespace's run lifecycle is
// published on. One per namespace, so a process subscribes to exactly the
// namespaces it has readers for.
func (b *RedisStreamBroker) runFeedChannel(namespace string) string {
	return b.prefix + "runs:" + namespace
}

// PublishRunEvent implements the RunFeed capability by publishing to Redis.
//
// Pub/sub rather than a stream: this is a notification, not the run. Every
// process holding a subscription is told at once, which is what lets a long
// poll answer the moment a run starts anywhere in the fleet — and there is
// nothing here worth persisting, since the run itself is already in the
// thread's stream and its history.
//
// What a subscription cannot do is tell a process what happened before it
// subscribed. That is why each process keeps a short window of what it heard
// (feedHub): a browser that reconnects is served from the window, and one that
// has been away longer than the window reconciles from the thread list.
func (b *RedisStreamBroker) PublishRunEvent(ctx context.Context, event RunEvent) error {
	if event.At.IsZero() {
		event.At = time.Now().UTC()
	}

	data, err := sonic.Marshal(event)
	if err != nil {
		return fmt.Errorf("failed to serialize run event: %w", err)
	}

	if err := b.client.Publish(ctx, b.runFeedChannel(event.Namespace), data).Err(); err != nil {
		return fmt.Errorf("failed to publish run event: %w", err)
	}
	return nil
}

// ReadRunEvents implements the RunFeed capability.
//
// It reads from this process's window rather than from Redis: the subscription
// below is what fills that window, and it is held once for the whole process
// however many browsers are watching. A hundred waiting long polls are a
// hundred goroutines parked on a mutex, not a hundred connections to Redis.
func (b *RedisStreamBroker) ReadRunEvents(
	ctx context.Context,
	namespaces []string,
	cursor string,
	wait time.Duration,
) ([]RunEvent, string, error) {
	b.ensureFeedSubscription(namespaces)

	events, next := b.runFeed.read(ctx, namespaces, ParseFeedCursor(cursor), wait)
	return events, FormatFeedCursor(next), nil
}

// ensureFeedSubscription starts listening to any namespace this process has
// not heard of yet.
//
// Subscribing on demand rather than up front, because the namespaces are the
// caller's to choose and nothing knows them in advance. A namespace subscribed
// to once stays subscribed: the set is small, the cost is a channel, and
// dropping it the moment the last reader leaves would only mean resubscribing
// when the next one arrives — with a gap in the middle where events are lost.
func (b *RedisStreamBroker) ensureFeedSubscription(namespaces []string) {
	b.feedMu.Lock()
	defer b.feedMu.Unlock()

	for _, namespace := range namespaces {
		if b.feedSubs[namespace] {
			continue
		}
		b.feedSubs[namespace] = true
		go b.consumeRunFeed(namespace)
	}
}

// consumeRunFeed relays one namespace's pub/sub channel into this process's
// window, until the broker is closed.
func (b *RedisStreamBroker) consumeRunFeed(namespace string) {
	sub := b.client.Subscribe(b.feedCtx, b.runFeedChannel(namespace))
	defer sub.Close()

	for {
		message, err := sub.ReceiveMessage(b.feedCtx)
		if err != nil {
			if b.feedCtx.Err() != nil {
				return
			}
			// A dropped connection is the one case worth reporting: events
			// published while it is down are gone, and a client that notices
			// a gap has only the thread list to fall back on.
			slog.Warn("run feed subscription interrupted; retrying",
				slog.String("namespace", namespace), slog.Any("error", err))
			select {
			case <-b.feedCtx.Done():
				return
			case <-time.After(time.Second):
			}
			continue
		}

		var event RunEvent
		if err := sonic.Unmarshal([]byte(message.Payload), &event); err != nil {
			slog.Warn("skipping unreadable run event",
				slog.String("namespace", namespace), slog.Any("error", err))
			continue
		}
		b.runFeed.append(event)
	}
}

// StopRunFeed ends this process's run feed subscriptions. It is for shutdown;
// a broker that is simply idle keeps listening, which is what lets a browser
// that reconnects be told what it missed.
func (b *RedisStreamBroker) StopRunFeed() {
	if b.feedStop != nil {
		b.feedStop()
	}
}
