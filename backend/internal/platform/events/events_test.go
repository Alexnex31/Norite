// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package events

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/internal/platform/redistest"
)

// eachBus runs a test against both implementations. That is the point of this file: the Redis half is a
// seam only the flagship activates, and until M18 nothing had ever run it (roadmap, M18). A property that
// holds for one implementation and not the other is a bug that appears only on the deployment with the
// most users.
func eachBus(t *testing.T, test func(t *testing.T, newBus func() Bus)) {
	t.Run("inproc", func(t *testing.T) {
		test(t, func() Bus {
			b := NewInProc(nil)
			t.Cleanup(func() { _ = b.Close() })
			return b
		})
	})
	t.Run("redis", func(t *testing.T) {
		url, prefix := redistest.URL(t), redistest.Namespace(t)
		test(t, func() Bus { return newRedisBus(t, url, prefix) })
	})
}

func newRedisBus(t *testing.T, url, prefix string) *Redis {
	t.Helper()
	b, err := NewRedis(context.Background(), RedisOptions{URL: url, Prefix: prefix})
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.Close() })
	return b
}

// collector records what a subscription received.
type collector struct {
	mu  sync.Mutex
	got []string
}

func (c *collector) handle(p []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.got = append(c.got, string(p))
}

func (c *collector) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.got...)
}

func (c *collector) waitFor(t *testing.T, n int) []string {
	t.Helper()
	require.Eventually(t, func() bool { return len(c.snapshot()) >= n }, 10*time.Second, 5*time.Millisecond,
		"expected %d messages", n)
	return c.snapshot()
}

func TestABusDeliversInOrder(t *testing.T) {
	eachBus(t, func(t *testing.T, newBus func() Bus) {
		bus := newBus()
		var c collector
		sub, err := bus.Subscribe("guild", c.handle)
		require.NoError(t, err)
		defer sub.Unsubscribe()

		var want []string
		for i := range 200 {
			msg := fmt.Sprintf("event-%03d", i)
			want = append(want, msg)
			require.NoError(t, bus.Publish(context.Background(), "guild", []byte(msg)))
		}
		assert.Equal(t, want, c.waitFor(t, len(want)))
	})
}

func TestEverySubscriberHearsAndTopicsDoNotMix(t *testing.T) {
	eachBus(t, func(t *testing.T, newBus func() Bus) {
		bus := newBus()
		var a, b, other collector
		for topic, c := range map[string]*collector{"guild": &a, "other": &other} {
			sub, err := bus.Subscribe(topic, c.handle)
			require.NoError(t, err)
			defer sub.Unsubscribe()
		}
		sub, err := bus.Subscribe("guild", b.handle)
		require.NoError(t, err)
		defer sub.Unsubscribe()

		require.NoError(t, bus.Publish(context.Background(), "guild", []byte("hello")))
		require.NoError(t, bus.Publish(context.Background(), "other", []byte("elsewhere")))

		assert.Equal(t, []string{"hello"}, a.waitFor(t, 1))
		assert.Equal(t, []string{"hello"}, b.waitFor(t, 1))
		assert.Equal(t, []string{"elsewhere"}, other.waitFor(t, 1))
	})
}

// The publisher owns its slice and may reuse it at once; a subscriber reading it later must see what was
// published, not what the buffer holds by then.
func TestAPublishedPayloadIsNotAliased(t *testing.T) {
	eachBus(t, func(t *testing.T, newBus func() Bus) {
		bus := newBus()
		block := make(chan struct{})
		var c collector
		sub, err := bus.Subscribe("guild", func(p []byte) { <-block; c.handle(p) })
		require.NoError(t, err)
		defer sub.Unsubscribe()

		buf := []byte("original")
		require.NoError(t, bus.Publish(context.Background(), "guild", buf))
		copy(buf, "OVERWRIT")
		close(block)

		assert.Equal(t, []string{"original"}, c.waitFor(t, 1))
	})
}

func TestUnsubscribeStopsDelivery(t *testing.T) {
	eachBus(t, func(t *testing.T, newBus func() Bus) {
		bus := newBus()
		var gone, stays collector
		sub, err := bus.Subscribe("guild", gone.handle)
		require.NoError(t, err)
		keep, err := bus.Subscribe("guild", stays.handle)
		require.NoError(t, err)
		defer keep.Unsubscribe()

		require.NoError(t, bus.Publish(context.Background(), "guild", []byte("before")))
		gone.waitFor(t, 1)
		sub.Unsubscribe()
		require.NoError(t, bus.Publish(context.Background(), "guild", []byte("after")))

		// The surviving subscriber hearing "after" is what makes the other one's silence evidence rather
		// than a message still in flight.
		assert.Equal(t, []string{"before", "after"}, stays.waitFor(t, 2))
		assert.Equal(t, []string{"before"}, gone.snapshot())
	})
}

// A publisher is an after-commit hook on somebody's request, so a subscriber that has stopped entirely must
// cost it nothing. Filling well past the buffer is what would block a naive implementation.
func TestASlowSubscriberNeverBlocksThePublisher(t *testing.T) {
	eachBus(t, func(t *testing.T, newBus func() Bus) {
		bus := newBus()
		release := make(chan struct{})
		sub, err := bus.Subscribe("guild", func([]byte) { <-release })
		require.NoError(t, err)

		start := time.Now()
		for range subscriptionBuffer + 500 {
			require.NoError(t, bus.Publish(context.Background(), "guild", []byte("x")))
		}
		assert.Less(t, time.Since(start), 5*time.Second, "publishing must not wait for a stalled subscriber")

		close(release)
		sub.Unsubscribe()
	})
}

func TestAClosedBusRefusesWork(t *testing.T) {
	eachBus(t, func(t *testing.T, newBus func() Bus) {
		bus := newBus()
		var c collector
		_, err := bus.Subscribe("guild", c.handle)
		require.NoError(t, err)
		require.NoError(t, bus.Close())

		assert.ErrorIs(t, bus.Publish(context.Background(), "guild", []byte("late")), ErrClosed)
		_, err = bus.Subscribe("guild", c.handle)
		assert.ErrorIs(t, err, ErrClosed)
		assert.NoError(t, bus.Close(), "closing twice is harmless")
	})
}

// What the Redis bus is for: two processes sharing a server hear each other. InProc cannot, by definition,
// which is why a flagship with more than one replica needs this implementation (M114).
func TestRedisCarriesEventsBetweenProcesses(t *testing.T) {
	url, prefix := redistest.URL(t), redistest.Namespace(t)
	publisher, subscriber := newRedisBus(t, url, prefix), newRedisBus(t, url, prefix)

	var c collector
	sub, err := subscriber.Subscribe("guild", c.handle)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	require.NoError(t, publisher.Publish(context.Background(), "guild", []byte("from another replica")))
	assert.Equal(t, []string{"from another replica"}, c.waitFor(t, 1))
}

func TestARedisURLIsNeverEchoed(t *testing.T) {
	_, err := NewRedis(context.Background(), RedisOptions{URL: "redis://user:hunter2@:::/bad"})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "hunter2")
}
