package mail

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// M5's "done when" asks for a reset that completes via a *real* SMTP relay, and this is the only file that
// can answer it. Everything else in this package tests the queue against a Sender that fails or blocks on
// request; nothing there would notice if SMTPSender spoke the protocol wrongly, addressed the envelope
// wrongly, or never dialed at all.
//
// Mailpit is a real SMTP server with an HTTP API for reading what it received, so the assertion is made
// from the relay's side rather than from the sender's.

// mailpit is a running relay plus the HTTP endpoint for inspecting its mailbox.
type mailpit struct {
	smtpHost string
	smtpPort int
	apiURL   string
}

func startMailpit(t *testing.T) mailpit {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping container-backed test in -short mode")
	}

	ctx := context.Background()
	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "axllent/mailpit:latest",
			ExposedPorts: []string{"1025/tcp", "8025/tcp"},
			// Both ports have to be listening: one to send to, one to read the result from.
			WaitingFor: wait.ForAll(
				wait.ForListeningPort("1025/tcp"),
				wait.ForListeningPort("8025/tcp"),
			).WithDeadline(90 * time.Second),
		},
		Started: true,
	})
	require.NoError(t, err, "could not start Mailpit")
	t.Cleanup(func() {
		if err := testcontainers.TerminateContainer(container); err != nil {
			t.Logf("could not terminate Mailpit: %v", err)
		}
	})

	host, err := container.Host(ctx)
	require.NoError(t, err)
	smtpPort, err := container.MappedPort(ctx, "1025/tcp")
	require.NoError(t, err)
	apiPort, err := container.MappedPort(ctx, "8025/tcp")
	require.NoError(t, err)

	return mailpit{
		smtpHost: host,
		smtpPort: int(smtpPort.Num()),
		apiURL:   fmt.Sprintf("http://%s:%d", host, apiPort.Num()),
	}
}

// received is the shape of Mailpit's message listing, narrowed to what these tests assert.
type received struct {
	Messages []struct {
		From    struct{ Address string } `json:"From"`
		To      []struct{ Address string }
		Subject string `json:"Subject"`
	} `json:"messages"`
	Total int `json:"messages_count"`
}

func (m mailpit) inbox(t *testing.T) received {
	t.Helper()

	resp, err := http.Get(m.apiURL + "/api/v1/messages")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	var out received
	require.NoError(t, json.Unmarshal(body, &out), "unexpected Mailpit response: %s", body)
	return out
}

// body fetches the plain-text part of the newest message, which is where the reset link lives.
func (m mailpit) latestBody(t *testing.T) string {
	t.Helper()

	resp, err := http.Get(m.apiURL + "/api/v1/message/latest")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	var out struct {
		Text string `json:"Text"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	return out.Text
}

// The end-to-end claim: a message handed to the queue reaches a real relay, correctly addressed, with its
// body intact. Mailpit speaks plain SMTP on 1025, so encryption is "none" here — the TLS modes are a
// property of the dialer, not of this path, and asserting them would need a relay with a certificate.
func TestSendingReachesARealRelay(t *testing.T) {
	relay := startMailpit(t)

	sender, err := NewSMTPSender(SMTPOptions{
		Host:        relay.smtpHost,
		Port:        relay.smtpPort,
		Encryption:  EncryptionNone,
		FromAddress: "no-reply@norite.test",
		FromName:    "Norite",
	})
	require.NoError(t, err)

	q := NewQueue(Options{Sender: sender, Logger: zerolog.New(io.Discard), Workers: 1})
	require.NoError(t, q.Enqueue(Message{
		Kind:    KindPasswordReset,
		To:      "ada@norite.test",
		Subject: "Reset your Norite password",
		Body:    "Open this link:\n\n    https://chat.example.com/reset?token=nrp_example\n",
	}))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	require.NoError(t, q.Shutdown(ctx), "the queue must drain before the relay is inspected")

	inbox := relay.inbox(t)
	require.Equal(t, 1, inbox.Total, "the relay should have received exactly one message")

	msg := inbox.Messages[0]
	assert.Equal(t, "no-reply@norite.test", msg.From.Address)
	require.Len(t, msg.To, 1)
	assert.Equal(t, "ada@norite.test", msg.To[0].Address)
	assert.Equal(t, "Reset your Norite password", msg.Subject)

	assert.Contains(t, relay.latestBody(t), "https://chat.example.com/reset?token=nrp_example",
		"the reset link must survive the trip through a real relay intact")
}

// M5's third "done when": sending must never block the HTTP response, even against a deliberately slow
// relay. Proven against a listener that accepts the connection and then says nothing at all, which is the
// worst realistic case — a relay that is reachable but wedged, where a naive client waits for its timeout.
func TestEnqueueReturnsImmediatelyAgainstAHangingRelay(t *testing.T) {
	// A socket that accepts and then never speaks. No container needed: the point is the dialer's
	// behavior, and a real relay cannot be asked to hang on demand.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = listener.Close() }()

	accepted := make(chan struct{}, 1)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			select {
			case accepted <- struct{}{}:
			default:
			}
			// Held open, deliberately silent: no SMTP banner, ever.
			_ = conn
		}
	}()

	host, portStr, err := net.SplitHostPort(listener.Addr().String())
	require.NoError(t, err)
	port, err := strconv.Atoi(portStr)
	require.NoError(t, err)

	sender, err := NewSMTPSender(SMTPOptions{
		Host: host, Port: port, Encryption: EncryptionNone, FromAddress: "no-reply@norite.test",
	})
	require.NoError(t, err)

	q := NewQueue(Options{
		Sender: sender, Logger: zerolog.New(io.Discard), Workers: 1, Attempts: 1,
		SendTimeout: 2 * time.Second,
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = q.Shutdown(ctx)
	})

	start := time.Now()
	require.NoError(t, q.Enqueue(Message{Kind: KindPasswordReset, To: "ada@norite.test", Subject: "x", Body: "y"}))
	elapsed := time.Since(start)

	// The handler's whole interaction with mail is this call. Anything but "immediately" here means a
	// password-reset request hangs for as long as the relay does.
	assert.Less(t, elapsed, 50*time.Millisecond,
		"Enqueue took %s against a hanging relay — the HTTP response would have waited with it", elapsed)

	// And the worker really did try, so the test is not passing because nothing happened.
	select {
	case <-accepted:
	case <-time.After(10 * time.Second):
		t.Fatal("the worker never dialed the relay")
	}
}

// A relay that rejects the envelope must not wedge the worker: the message is dropped after its attempts
// and the queue keeps moving. Otherwise one bad address stops delivery for everything behind it.
func TestARejectedMessageDoesNotBlockTheQueue(t *testing.T) {
	relay := startMailpit(t)

	sender, err := NewSMTPSender(SMTPOptions{
		Host: relay.smtpHost, Port: relay.smtpPort, Encryption: EncryptionNone,
		FromAddress: "no-reply@norite.test",
	})
	require.NoError(t, err)

	q := NewQueue(Options{
		Sender: sender, Logger: zerolog.New(io.Discard), Workers: 1, Attempts: 1,
		Backoff: time.Millisecond,
	})

	// An address the library refuses to put in an envelope at all.
	require.NoError(t, q.Enqueue(Message{Kind: KindPasswordReset, To: "not an address", Subject: "x", Body: "y"}))
	// ...followed by a good one, which must still arrive.
	require.NoError(t, q.Enqueue(Message{
		Kind: KindPasswordReset, To: "ada@norite.test", Subject: "delivered anyway", Body: "z",
	}))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	require.NoError(t, q.Shutdown(ctx))

	inbox := relay.inbox(t)
	require.Equal(t, 1, inbox.Total, "the good message must arrive despite the bad one ahead of it")
	assert.Equal(t, "delivered anyway", inbox.Messages[0].Subject)
	assert.False(t, strings.Contains(inbox.Messages[0].Subject, "x"))
}
