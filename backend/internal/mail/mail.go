// Package mail sends transactional email, always off the request path.
//
// # The one rule this package exists to enforce
//
// **Sending never blocks an HTTP response** (docs/adr/0020-operations.md, docs/architecture.md §2). A relay
// is a third party over a network: it can be slow, wedged, or greylisting, and none of that is a reason for
// a password-reset request to hang. Enqueue therefore hands a message to a background worker and returns
// immediately — it cannot block, by construction, because a full queue drops rather than waits.
//
// That property is also what makes the reset endpoint's anti-enumeration guarantee cheap. If sending
// happened inline, a request for a registered address would take as long as an SMTP transaction and one for
// an unknown address would return instantly, and the endpoint would leak through timing no matter what its
// response body said. With sending detached, both answers are the same work.
//
// # Delivery is best-effort, deliberately
//
// The queue is in memory. A message still queued when the process stops is drained if it can be and dropped
// if it cannot, and nothing survives a crash. That is the accepted trade for M5: the only sender is
// password reset, where a lost message means the user requests another, and a durable outbox is
// operational surface that should not be built before there is load to justify it
// (docs/architecture.md §15.7). If a future sender needs at-least-once delivery, the upgrade is a Postgres
// outbox table drained by this same worker loop — Enqueue's signature does not change.
package mail

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"github.com/wneessen/go-mail"
)

// Kind labels a message for logging. It is what appears in a log line instead of the recipient or the body,
// both of which are personal data that CLAUDE.md rule 8's reasoning covers even though neither is a secret.
type Kind string

const (
	// KindPasswordReset is M5's only sender.
	KindPasswordReset Kind = "password_reset"
)

// Message is one outbound email.
//
// Plain text only, and not for want of effort: a reset mail needs no formatting, text renders identically
// in every client including the terminal ones this project's users are likely to hold it open in, and an
// HTML body is one more place for a link to be rewritten on the way through.
type Message struct {
	Kind    Kind
	To      string
	Subject string
	Body    string
}

// Sender delivers a single message and returns when the relay has accepted or refused it.
//
// An interface because the queue's behavior — that it does not block, that it retries, that it drains —
// has to be testable against a relay that is slow or broken on demand, which no real SMTP server obliges
// with. The production implementation is SMTPSender; tests supply their own.
type Sender interface {
	Send(ctx context.Context, msg Message) error
}

// ErrDisabled is returned by Enqueue when the instance has no relay configured.
//
// Callers are expected to turn this into "this instance cannot send email" rather than swallowing it: an
// operator who never configured SMTP has not chosen for password reset to fail silently.
var ErrDisabled = errors.New("mail: no SMTP relay is configured on this instance")

// Options configures a Queue.
type Options struct {
	// Sender delivers messages. Nil means the instance has no relay, and Enqueue reports ErrDisabled.
	Sender Sender
	Logger zerolog.Logger

	// Workers is how many messages may be in flight at once. Small on purpose: relays rate-limit, and a
	// burst of parallel connections from one instance is what gets a sender throttled or blocked.
	Workers int
	// Capacity bounds the backlog. Reached only when the relay is slower than the request rate for a
	// sustained period, at which point dropping is the honest failure — the alternative is growing until
	// the process dies, taking messaging and presence with it.
	Capacity int

	// Attempts is how many times a message is tried before it is dropped, and Backoff is the wait after
	// the first failure, doubling each time.
	Attempts int
	Backoff  time.Duration

	// SendTimeout bounds one delivery attempt, so a relay that accepts a connection and then stops talking
	// occupies a worker for a bounded time rather than forever.
	SendTimeout time.Duration
}

func (o *Options) setDefaults() {
	if o.Workers <= 0 {
		o.Workers = 2
	}
	if o.Capacity <= 0 {
		o.Capacity = 256
	}
	if o.Attempts <= 0 {
		o.Attempts = 3
	}
	if o.Backoff <= 0 {
		o.Backoff = 2 * time.Second
	}
	if o.SendTimeout <= 0 {
		o.SendTimeout = 30 * time.Second
	}
}

// Queue accepts messages and delivers them on background workers.
type Queue struct {
	opts Options
	ch   chan Message
	wg   sync.WaitGroup

	// closeOnce guards the channel close so a second Shutdown — which a failed startup path can easily
	// produce — is a no-op rather than a panic on a closed channel.
	closeOnce sync.Once
	// stopped is closed when the queue stops accepting, so Enqueue can refuse without racing on the send.
	stopped chan struct{}

	// now is overridable so backoff can be tested without waiting for it.
	sleep func(context.Context, time.Duration)
}

// NewQueue starts the workers and returns a queue ready to accept messages.
//
// A queue with a nil Sender is valid and useful: it is what an instance with SMTP switched off runs, and it
// reports ErrDisabled rather than being nil itself, so no caller has to nil-check a dependency.
func NewQueue(opts Options) *Queue {
	opts.setDefaults()

	q := &Queue{
		opts:    opts,
		ch:      make(chan Message, opts.Capacity),
		stopped: make(chan struct{}),
		sleep:   sleepCtx,
	}
	if opts.Sender == nil {
		return q // nothing to run: Enqueue refuses before it ever reaches a worker
	}

	q.wg.Add(opts.Workers)
	for range opts.Workers {
		go q.worker()
	}
	return q
}

// Enabled reports whether this instance can send email at all.
func (q *Queue) Enabled() bool { return q.opts.Sender != nil }

// Enqueue accepts a message for delivery. It never blocks.
//
// The three outcomes are all deliberate and all non-blocking: ErrDisabled when there is no relay,
// ErrQueueFull when the backlog is saturated, and nil when the message is accepted — which promises only
// that delivery will be *attempted*, never that it succeeded.
func (q *Queue) Enqueue(msg Message) error {
	if !q.Enabled() {
		return ErrDisabled
	}

	select {
	case <-q.stopped:
		return ErrDisabled
	default:
	}

	select {
	case q.ch <- msg:
		return nil
	default:
		// Deliberately not blocking, and deliberately not growing. Logged at warn because a saturated
		// queue means mail is being lost, which is worth an alert even though it must not fail the
		// request that produced it.
		q.opts.Logger.Warn().
			Str("kind", string(msg.Kind)).
			Int("capacity", q.opts.Capacity).
			Msg("mail queue is full — message dropped")
		return ErrQueueFull
	}
}

// ErrQueueFull means the backlog was saturated and the message was dropped.
var ErrQueueFull = errors.New("mail: send queue is full")

// Shutdown stops accepting messages and waits for the backlog to drain, giving up when ctx expires.
//
// Called from the server's shutdown path after the HTTP listener has stopped, so nothing new arrives while
// this runs. Returning early on a deadline is intentional: a wedged relay must not hold the process open
// past its shutdown timeout, and an undelivered reset email is a smaller problem than a container that
// will not stop.
func (q *Queue) Shutdown(ctx context.Context) error {
	q.closeOnce.Do(func() {
		close(q.stopped)
		close(q.ch)
	})

	done := make(chan struct{})
	go func() {
		q.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		remaining := len(q.ch)
		q.opts.Logger.Warn().
			Int("undelivered", remaining).
			Msg("mail queue did not drain before shutdown deadline")
		return ctx.Err()
	}
}

// worker delivers messages until the queue is closed and drained.
func (q *Queue) worker() {
	defer q.wg.Done()
	for msg := range q.ch {
		q.deliver(msg)
	}
}

// deliver makes up to Attempts tries at one message, then gives up.
func (q *Queue) deliver(msg Message) {
	// Detached from any request context — the request this message came from returned long ago, and
	// inheriting its cancellation would mean a client hanging up canceled their own reset email.
	backoff := q.opts.Backoff

	for attempt := 1; attempt <= q.opts.Attempts; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), q.opts.SendTimeout)
		err := q.opts.Sender.Send(ctx, msg)
		cancel()

		if err == nil {
			// Recipient deliberately absent: it is an email address, and this line exists to show that
			// delivery works, not who is using the instance.
			q.opts.Logger.Debug().
				Str("kind", string(msg.Kind)).
				Int("attempt", attempt).
				Msg("message delivered")
			return
		}

		if attempt == q.opts.Attempts {
			q.opts.Logger.Error().Err(err).
				Str("kind", string(msg.Kind)).
				Int("attempts", attempt).
				Msg("giving up on message after repeated delivery failures")
			return
		}

		q.opts.Logger.Warn().Err(err).
			Str("kind", string(msg.Kind)).
			Int("attempt", attempt).
			Dur("retry_in", backoff).
			Msg("delivery failed — retrying")

		q.sleep(context.Background(), backoff)
		backoff *= 2
	}
}

// sleepCtx waits for d, or until ctx is done.
func sleepCtx(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
	}
}

// ---------- the real relay ----------

// Encryption names how the connection to the relay is protected.
type Encryption string

const (
	// EncryptionStartTLS upgrades a plaintext connection, which is what submission relays on 587 speak.
	EncryptionStartTLS Encryption = "starttls"
	// EncryptionTLS is implicit TLS, usually port 465.
	EncryptionTLS Encryption = "tls"
	// EncryptionNone is plaintext. Only defensible for a relay reached over loopback or a container
	// network; across anything else it hands the relay credential to whoever is on the path.
	EncryptionNone Encryption = "none"
)

// SMTPOptions configures the real sender.
type SMTPOptions struct {
	Host        string
	Port        int
	Username    string
	Password    string
	Encryption  Encryption
	FromAddress string
	FromName    string
}

// SMTPSender delivers through a real relay.
type SMTPSender struct {
	client *mail.Client
	opts   SMTPOptions
}

// NewSMTPSender builds a sender, validating the relay settings eagerly.
//
// Eagerly because the alternative is discovering a typo in the host name at the moment someone first needs
// a password reset. Startup is where configuration errors belong.
func NewSMTPSender(opts SMTPOptions) (*SMTPSender, error) {
	if opts.Host == "" {
		return nil, errors.New("mail: an SMTP host is required")
	}
	if opts.FromAddress == "" {
		return nil, errors.New("mail: an SMTP from address is required")
	}

	clientOpts := []mail.Option{mail.WithPort(opts.Port), mail.WithTimeout(30 * time.Second)}

	switch opts.Encryption {
	case EncryptionTLS:
		clientOpts = append(clientOpts, mail.WithSSL())
	case EncryptionNone:
		clientOpts = append(clientOpts, mail.WithTLSPolicy(mail.NoTLS))
	case EncryptionStartTLS, "":
		// Mandatory, not opportunistic. Opportunistic STARTTLS falls back to plaintext when the server
		// says it cannot upgrade, which is indistinguishable from an attacker stripping the capability —
		// so the "encrypted" setting would silently send the credential in the clear.
		clientOpts = append(clientOpts, mail.WithTLSPolicy(mail.TLSMandatory))
	default:
		return nil, fmt.Errorf("mail: unknown encryption %q", opts.Encryption)
	}

	if opts.Username != "" {
		clientOpts = append(clientOpts,
			mail.WithSMTPAuth(mail.SMTPAuthAutoDiscover),
			mail.WithUsername(opts.Username),
			mail.WithPassword(opts.Password),
		)
	}

	client, err := mail.NewClient(opts.Host, clientOpts...)
	if err != nil {
		// Never wrap the raw error without care: go-mail's messages are safe here, but the password is in
		// opts and must not find its way into one.
		return nil, fmt.Errorf("mail: building SMTP client for %s: %w", opts.Host, err)
	}

	return &SMTPSender{client: client, opts: opts}, nil
}

// Send delivers one message synchronously.
func (s *SMTPSender) Send(ctx context.Context, msg Message) error {
	m := mail.NewMsg()

	if s.opts.FromName != "" {
		if err := m.FromFormat(s.opts.FromName, s.opts.FromAddress); err != nil {
			return fmt.Errorf("mail: setting sender: %w", err)
		}
	} else if err := m.From(s.opts.FromAddress); err != nil {
		return fmt.Errorf("mail: setting sender: %w", err)
	}

	if err := m.To(msg.To); err != nil {
		// The address came from a user record, so it was valid when it was stored; this is a programming
		// error or a corrupt row rather than routine input rejection.
		return fmt.Errorf("mail: setting recipient: %w", err)
	}

	m.Subject(msg.Subject)
	m.SetBodyString(mail.TypeTextPlain, msg.Body)

	if err := s.client.DialAndSendWithContext(ctx, m); err != nil {
		return fmt.Errorf("mail: sending via %s: %w", s.opts.Host, err)
	}
	return nil
}
