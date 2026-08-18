package mail

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests are about the queue's behavior under a relay that misbehaves, which is the whole reason the
// Sender is an interface: a real SMTP server will not hang or fail three times on request.

func testMessage() Message {
	return Message{Kind: KindPasswordReset, To: "ada@example.com", Subject: "Reset", Body: "link"}
}

// recordingSender counts deliveries and can be told to fail or block.
type recordingSender struct {
	mu       sync.Mutex
	received []Message

	failures atomic.Int32 // fail this many times before succeeding
	block    chan struct{}
	started  chan struct{}
	// beforeSend runs at the top of every attempt, so a test can act in the middle of one.
	beforeSend func()
}

func (s *recordingSender) Send(ctx context.Context, msg Message) error {
	if s.started != nil {
		select {
		case s.started <- struct{}{}:
		default:
		}
	}
	if s.beforeSend != nil {
		s.beforeSend()
	}
	if s.block != nil {
		select {
		case <-s.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if s.failures.Load() > 0 {
		s.failures.Add(-1)
		return errors.New("relay refused")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.received = append(s.received, msg)
	return nil
}

func (s *recordingSender) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.received)
}

func newQueue(t *testing.T, opts Options) *Queue {
	t.Helper()
	opts.Logger = zerolog.New(io.Discard)
	// Backoff is exercised as a decision, not as real waiting: a test that actually slept would be slow
	// and would still not prove anything the recorded delay does not.
	q := NewQueue(opts)
	q.sleep = func(context.Context, time.Duration) {}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = q.Shutdown(ctx)
	})
	return q
}

// The rule the package exists for. A relay that never answers must not hold up the caller — Enqueue is
// what the HTTP handler calls, so if it can block, the response can block with it.
func TestEnqueueDoesNotBlockOnAWedgedRelay(t *testing.T) {
	sender := &recordingSender{block: make(chan struct{})}
	defer close(sender.block)

	q := newQueue(t, Options{Sender: sender, Workers: 1, Capacity: 4})

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 4 {
			_ = q.Enqueue(testMessage())
		}
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Enqueue blocked while the relay was unresponsive — a reset request would hang with it")
	}
}

// A saturated queue drops. Growing instead would trade a lost email for a dead process, taking messaging
// and presence down with it.
func TestAFullQueueDropsRatherThanGrows(t *testing.T) {
	sender := &recordingSender{block: make(chan struct{}), started: make(chan struct{}, 1)}
	defer close(sender.block)

	q := newQueue(t, Options{Sender: sender, Workers: 1, Capacity: 2})

	// Let one message reach the blocked worker so the buffer, not the worker, is what fills.
	require.NoError(t, q.Enqueue(testMessage()))
	<-sender.started

	require.NoError(t, q.Enqueue(testMessage()))
	require.NoError(t, q.Enqueue(testMessage()))

	err := q.Enqueue(testMessage())
	require.ErrorIs(t, err, ErrQueueFull, "the fourth message must be refused, not buffered")
}

func TestDeliveryRetriesThenGivesUp(t *testing.T) {
	t.Run("succeeds on a later attempt", func(t *testing.T) {
		sender := &recordingSender{}
		sender.failures.Store(2)

		q := newQueue(t, Options{Sender: sender, Workers: 1, Attempts: 3})
		require.NoError(t, q.Enqueue(testMessage()))

		assert.Eventually(t, func() bool { return sender.count() == 1 }, 3*time.Second, 10*time.Millisecond,
			"a message that fails twice must still be delivered on the third attempt")
	})

	t.Run("gives up rather than retrying forever", func(t *testing.T) {
		sender := &recordingSender{}
		sender.failures.Store(100)

		q := newQueue(t, Options{Sender: sender, Workers: 1, Attempts: 3})
		require.NoError(t, q.Enqueue(testMessage()))

		// After Attempts failures the worker must move on. If it looped forever the queue would wedge on
		// one bad address and stop delivering everything behind it.
		assert.Eventually(t, func() bool { return sender.failures.Load() == 97 }, 3*time.Second, 10*time.Millisecond,
			"exactly Attempts tries should have been made")

		time.Sleep(50 * time.Millisecond)
		assert.Equal(t, int32(97), sender.failures.Load(), "no further attempts after giving up")
		assert.Zero(t, sender.count())
	})
}

// Shutdown drains what is already queued, so a deploy does not silently swallow the reset email someone
// requested a second earlier.
func TestShutdownDrainsTheBacklog(t *testing.T) {
	sender := &recordingSender{}
	q := NewQueue(Options{Sender: sender, Logger: zerolog.New(io.Discard), Workers: 1, Capacity: 16})

	for range 8 {
		require.NoError(t, q.Enqueue(testMessage()))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, q.Shutdown(ctx))

	assert.Equal(t, 8, sender.count(), "everything already accepted must be delivered before shutdown returns")
}

// ...but a wedged relay must not hold the process open past its shutdown deadline. A container that will
// not stop is a worse outcome than an undelivered email.
func TestShutdownGivesUpOnADeadline(t *testing.T) {
	sender := &recordingSender{block: make(chan struct{})}
	defer close(sender.block)

	q := NewQueue(Options{Sender: sender, Logger: zerolog.New(io.Discard), Workers: 1, Capacity: 4})
	require.NoError(t, q.Enqueue(testMessage()))

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := q.Shutdown(ctx)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(start), 2*time.Second, "Shutdown must respect its deadline, not the relay's pace")
}

// Shutdown is called from a path that can run twice — a failed startup unwinds through it — so a second
// call must be a no-op rather than a panic on an already-closed channel.
// Canceling the backoff is not the same as stopping the retries, and the first version of this fix
// confused the two: the wait ended immediately on shutdown and the loop went straight into the next attempt
// with a full send timeout of its own, so a wedged relay held the stop for nearly as long as before — and
// the retries, no longer spaced by a backoff, arrived back to back.
func TestShutdownStopsRetryingRatherThanRetryingFaster(t *testing.T) {
	sender := &recordingSender{}
	sender.failures.Store(10) // never succeeds

	q := newQueue(t, Options{Sender: sender, Attempts: 5, Backoff: time.Millisecond})

	// Count attempts, and stop the queue from inside the first one — the moment the retry decision is
	// about to be made for real.
	var attempts atomic.Int32
	stopped := make(chan struct{})
	q.sleep = func(context.Context, time.Duration) {}
	sender.beforeSend = func() {
		if attempts.Add(1) == 1 {
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = q.Shutdown(ctx)
				close(stopped)
			}()
			// Give Shutdown time to close the channel and cancel, so the retry decision is made against a
			// queue that is genuinely stopping rather than one that merely will be.
			time.Sleep(20 * time.Millisecond)
		}
	}

	require.NoError(t, q.Enqueue(Message{Kind: KindPasswordReset, To: "ada@example.com"}))
	<-stopped

	assert.LessOrEqual(t, int(attempts.Load()), 2,
		"a stopping queue must give up on the message, not spend its remaining attempts on it")
}

func TestShutdownIsIdempotent(t *testing.T) {
	q := NewQueue(Options{Sender: &recordingSender{}, Logger: zerolog.New(io.Discard)})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	require.NoError(t, q.Shutdown(ctx))
	assert.NotPanics(t, func() { _ = q.Shutdown(ctx) })
}

// An instance with no relay is a working instance. The queue is still a real object so no caller has to
// nil-check it — it simply refuses, with an error that says why.
func TestADisabledQueueRefusesClearly(t *testing.T) {
	q := NewQueue(Options{Logger: zerolog.New(io.Discard)})

	assert.False(t, q.Enabled())
	assert.ErrorIs(t, q.Enqueue(testMessage()), ErrDisabled)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	assert.NoError(t, q.Shutdown(ctx), "shutting down a disabled queue must not hang or error")
}

// Enqueue after shutdown refuses rather than panicking on a closed channel — the shutdown path stops the
// listener first, but an in-flight request can still reach this.
func TestEnqueueAfterShutdownRefuses(t *testing.T) {
	q := NewQueue(Options{Sender: &recordingSender{}, Logger: zerolog.New(io.Discard)})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, q.Shutdown(ctx))

	assert.NotPanics(t, func() {
		assert.Error(t, q.Enqueue(testMessage()))
	})
}

func TestNewSMTPSenderValidatesEagerly(t *testing.T) {
	_, err := NewSMTPSender(SMTPOptions{FromAddress: "no-reply@example.com"})
	assert.Error(t, err, "a missing host must fail at construction, not at the first reset request")

	_, err = NewSMTPSender(SMTPOptions{Host: "smtp.example.com"})
	assert.Error(t, err, "a missing from address must fail at construction")

	_, err = NewSMTPSender(SMTPOptions{
		Host: "smtp.example.com", Port: 587, FromAddress: "no-reply@example.com", Encryption: "quantum",
	})
	assert.Error(t, err, "an unknown encryption mode must be rejected rather than silently defaulted")

	for _, enc := range []Encryption{EncryptionStartTLS, EncryptionTLS, EncryptionNone, ""} {
		_, err := NewSMTPSender(SMTPOptions{
			Host: "smtp.example.com", Port: 587, FromAddress: "no-reply@example.com", Encryption: enc,
		})
		assert.NoError(t, err, "encryption %q must be accepted", enc)
	}
}
