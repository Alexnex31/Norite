// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package events

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Redis carries the bus over Redis (or Valkey) pub/sub, so every process connected to the same server sees
// every publication. It is what lets the flagship run more than one replica (M114).
type Redis struct {
	client *redis.Client
	prefix string

	mu     sync.Mutex
	subs   map[*redisSub]struct{}
	closed bool
}

// RedisOptions configures NewRedis.
type RedisOptions struct {
	// URL is a redis:// or rediss:// URL. It may carry a password, so it is never echoed into an error or a
	// log line (rule 8).
	URL string
	// Prefix namespaces every channel, so one server can be shared and so tests running concurrently against
	// one container do not hear each other. Defaults to "norite:events:".
	Prefix string
	// ConnectTimeout bounds the startup round trip. Defaults to ten seconds.
	ConnectTimeout time.Duration
}

// NewRedis connects and verifies the connection with a round trip, for database.New's reason: a client
// connects lazily, so without the ping a wrong URL surfaces as a failure on the first event rather than as
// a startup error naming the setting.
func NewRedis(ctx context.Context, opts RedisOptions) (*Redis, error) {
	parsed, err := redis.ParseURL(opts.URL)
	if err != nil {
		return nil, errors.New("events: could not parse the configured Redis URL")
	}
	prefix := opts.Prefix
	if prefix == "" {
		prefix = "norite:events:"
	}
	timeout := opts.ConnectTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	client := redis.NewClient(parsed)
	pingCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := client.Ping(pingCtx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("events: could not reach Redis within %s: %w", timeout, err)
	}
	return &Redis{client: client, prefix: prefix, subs: map[*redisSub]struct{}{}}, nil
}

// Publish sends payload to the topic's channel on the server.
func (b *Redis) Publish(ctx context.Context, topic string, payload []byte) error {
	b.mu.Lock()
	closed := b.closed
	b.mu.Unlock()
	if closed {
		return ErrClosed
	}
	if err := b.client.Publish(ctx, b.prefix+topic, payload).Err(); err != nil {
		return fmt.Errorf("events: publishing to %q: %w", topic, err)
	}
	return nil
}

// Subscribe registers handle for topic, returning only once the server has confirmed the subscription.
//
// go-redis writes SUBSCRIBE before returning but does not wait for the server's reply, and pub/sub keeps
// nothing for a listener that arrives late. Without the wait, "from the moment Subscribe returns" would hold
// only because a publish on another connection happened to reach the server after the subscribe did.
// Waiting makes it a guarantee. The race is too narrow to provoke: with this wait removed, the ordering and
// cross-process tests passed five runs out of five. So it is kept on its reasoning rather than on a test,
// and says so rather than implying one exists.
func (b *Redis) Subscribe(topic string, handle func(payload []byte)) (Subscription, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil, ErrClosed
	}

	ctx := context.Background()
	ps := b.client.Subscribe(ctx, b.prefix+topic)
	if _, err := ps.Receive(ctx); err != nil {
		_ = ps.Close()
		return nil, fmt.Errorf("events: subscribing to %q: %w", topic, err)
	}

	s := &redisSub{bus: b, ps: ps, done: make(chan struct{})}
	// A full channel drops after the send timeout rather than stalling the connection's reader: the same
	// at-most-once behavior InProc has, and the one the package comment promises.
	ch := ps.Channel(redis.WithChannelSize(subscriptionBuffer), redis.WithChannelSendTimeout(time.Second))
	go func() {
		defer close(s.done)
		for msg := range ch {
			handle([]byte(msg.Payload))
		}
	}()
	b.subs[s] = struct{}{}
	return s, nil
}

// Close stops every subscription and closes the client.
func (b *Redis) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	subs := b.subs
	b.subs = map[*redisSub]struct{}{}
	b.mu.Unlock()

	for s := range subs {
		s.halt()
	}
	return b.client.Close()
}

type redisSub struct {
	bus  *Redis
	ps   *redis.PubSub
	once sync.Once
	done chan struct{}
}

// Unsubscribe closes the subscription and waits for its handler to finish.
func (s *redisSub) Unsubscribe() {
	s.bus.mu.Lock()
	delete(s.bus.subs, s)
	s.bus.mu.Unlock()
	s.halt()
}

func (s *redisSub) halt() {
	s.once.Do(func() { _ = s.ps.Close() })
	<-s.done
}
