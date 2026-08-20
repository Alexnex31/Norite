package login

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Alexnex31/Norite/cli/internal/termsafe"
)

// The listener a browser returns to once the instance has finished with the provider.
//
// # What actually happens
//
// The provider redirects to the *instance*, which holds the client secret and does the token exchange
// (ADR 0024). The instance then redirects here, carrying a single-use exchange code. Nothing about this
// listener is registered with Google or GitHub, and it does not have to be: they never see it.
//
// That is worth stating because the plan said the opposite for a long time — that the port was registered
// with the provider, which would mean this binary holding a client secret. It never did. See ADR 0027.
//
// # What a squatter on one of these ports gets
//
// Nothing usable. The code is bound to a flow verifier that exists only in this process's memory and is
// never published, so a process that receives one cannot redeem it. That is what makes walking past an
// occupied port safe rather than merely convenient.

// loopbackPorts are the ports `norite login` will bind, in order.
//
// Fixed and published rather than ephemeral, so the port somebody has to allow through a local firewall is
// knowable in advance and can be written down. The instance validates the *host*, not the port, so this
// list is a convention of this client's own and costs nothing to change.
var loopbackPorts = []int{51763, 51764, 51765, 51766, 51767}

// loopbackPath is the one path the listener answers. Everything else is a 404, so a stray request from
// some other local program cannot be mistaken for a callback.
const loopbackPath = "/callback"

// loopbackTimeout is how long to wait for the browser leg.
//
// Matched to the 15-minute authorization-state TTL that contracts/openapi.yaml documents. Giving up sooner
// would abandon a sign-in the instance would still have completed — a provider's second factor is
// unhurried, and a person who has just been asked for a hardware key is not hurrying either. Duplicated
// rather than imported, for the reason mintFlowBinding gives.
const loopbackTimeout = 15 * time.Minute

// errLoopbackTimedOut is the wait expiring with nothing having arrived.
var errLoopbackTimedOut = errors.New(
	"timed out waiting for the sign-in to finish in your browser; run `norite login` again")

// callbackResult is what arrived at the listener. Exactly one field is set.
type callbackResult struct {
	// code is an exchange code. Never printed and never logged (rule 8).
	code string
	// failure is a code from the instance's fixed vocabulary, already reduced to something safe.
	failure string
}

// loopback is a bound listener waiting for one callback.
type loopback struct {
	listener net.Listener
	server   *http.Server
	result   chan callbackResult
	once     sync.Once
}

// listenLoopback binds the first available port from ports and starts serving.
func listenLoopback(ports []int) (*loopback, error) {
	l := &loopback{result: make(chan callbackResult, 1)}

	var tried []string
	for _, port := range ports {
		// The address literal, never ":port" and never "localhost:port". ":port" binds 0.0.0.0 and puts a
		// sign-in listener on the LAN, which is the one mistake in this file that would actually matter.
		// A name would be resolved, and the instance refuses a name in a return URI for that reason.
		listener, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
		if err == nil {
			l.listener = listener
			break
		}
		if errors.Is(err, os.ErrPermission) {
			// Reported rather than walked past: "free this port and retry" is not the fix for a permission
			// problem, and a wrong suggestion costs somebody an hour. Should not arise on ports above
			// 1024, but a hardened host or a container policy can produce it.
			return nil, fmt.Errorf("not allowed to listen on 127.0.0.1:%d: %w", port, err)
		}
		// Every other failure is treated the same and not diagnosed. A bind error says something holds the
		// port and nothing about what, and finding out would mean opening a connection to an unknown local
		// service and speaking HTTP at it — racy, and rude.
		tried = append(tried, strconv.Itoa(port))
	}

	if l.listener == nil {
		return nil, fmt.Errorf(
			"every port `norite login` can use is busy (tried %s on 127.0.0.1); "+
				"free one of them and run it again, or sign in with a password instead",
			strings.Join(tried, ", "))
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", l.handle)
	l.server = &http.Server{
		Handler: mux,
		// gosec G112, and cheap regardless: a local process that opens a connection and never finishes a
		// request must not be able to hold this listener open for the whole fifteen minutes.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}

	go func() { _ = l.server.Serve(l.listener) }()
	return l, nil
}

// redirectURI is what to send to /authorize. Built from the address actually bound, not from the port that
// was asked for, so a fallback cannot produce a URI pointing at a port nothing is listening on.
func (l *loopback) redirectURI() string {
	return "http://" + l.listener.Addr().String() + loopbackPath
}

// handle answers the browser.
func (l *loopback) handle(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != loopbackPath {
		http.NotFound(w, r)
		return
	}

	delivered := false
	l.once.Do(func() {
		delivered = true
		l.result <- l.read(r)
	})

	// Only the first callback counts. A second one is a refresh, a prefetch, or something local being
	// curious; none of them may replace a result already in hand.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-store")
	if delivered {
		_, _ = w.Write([]byte(loopbackDonePage))
		return
	}
	_, _ = w.Write([]byte(loopbackAlreadyDonePage))
}

// read turns the browser's query into a result.
//
// This is the boundary rule 19 is about, and it is a new one: everything else the CLI reads comes from the
// instance over TLS, while this arrives on a socket any local process on the machine can write to. It is
// handled by *vocabulary* rather than by sanitizing — the values are matched against a fixed set and
// mapped to this program's own wording, so nothing a stranger wrote is ever printed.
func (l *loopback) read(r *http.Request) callbackResult {
	query := r.URL.Query()

	if code := query.Get("code"); code != "" {
		// Shape-checked here rather than at the instance. It costs a comparison, and it means a local
		// process cannot make this CLI relay arbitrary bytes upstream in an authenticated-looking request.
		if !looksLikeExchangeCode(code) {
			return callbackResult{failure: "malformed_code"}
		}
		return callbackResult{code: code}
	}

	return callbackResult{failure: reduceFailure(query.Get("error"))}
}

// looksLikeExchangeCode reports whether a value has the shape contracts/openapi.yaml describes: the `noc_`
// prefix and 43 base64url characters.
func looksLikeExchangeCode(code string) bool {
	body, ok := strings.CutPrefix(code, "noc_")
	if !ok || len(body) != 43 {
		return false
	}
	for _, r := range body {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

// reduceFailure narrows whatever arrived to something safe to keep.
//
// A known code is kept as-is. Anything else is bounded and stripped to the characters an OAuth error code
// can contain — a whitelist rather than a filter, which is the right way round for a value with a known
// shape. termsafe runs first anyway, so this is two independent reasons the value is inert.
//
// `error_description` is deliberately never read, even if a future instance sends one: the contract says
// there is no such parameter, and a free-form sentence from a socket like this one is exactly what the
// fixed vocabulary exists to avoid.
func reduceFailure(raw string) string {
	if raw == "" {
		return "no_result"
	}
	switch raw {
	case "access_denied", "email_unverified", "identity_linked_elsewhere",
		"account_already_linked", "registration_closed", "email_taken", "no_email", "server_error":
		return raw
	}

	safe := termsafe.Text(raw)
	var b strings.Builder
	for _, r := range safe {
		if b.Len() == 64 {
			break
		}
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "server_error"
	}
	return b.String()
}

// wait blocks until the browser comes back, the caller gives up, or the timeout expires.
//
// The ctx case is what makes this callable from anywhere: a test canceling t.Context() today, and the
// TUI's login screen later. It is not what handles Ctrl-C — cmd/app exits on a signal without unwinding,
// and the kernel releases the listening socket — but a wait that only ever returns on a callback or a
// fifteen-minute timer would be untestable, which is the more immediate problem.
func (l *loopback) wait(ctx context.Context) (callbackResult, error) {
	timeout := time.NewTimer(loopbackTimeout)
	defer timeout.Stop()

	select {
	case res := <-l.result:
		return res, nil
	case <-ctx.Done():
		return callbackResult{}, ctx.Err()
	case <-timeout.C:
		return callbackResult{}, errLoopbackTimedOut
	}
}

// Close stops the listener. Errors are dropped: this runs on the way out of a flow that has already
// succeeded or already failed, and there is nothing a caller could do about a socket that will be closed by
// process exit moments later anyway.
func (l *loopback) Close() error {
	_ = l.server.Close()
	return nil
}

// The two pages the listener serves. Constants with no interpolation of any kind and no subresources: zero
// injection surface by construction rather than by escaping, and nothing to fetch means nothing that could
// carry the code away in a Referer.
const loopbackDonePage = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>Signed in</title></head>
<body style="font-family: system-ui, sans-serif; max-width: 32rem; margin: 4rem auto; padding: 0 1.5rem">
<h1>Signed in</h1>
<p>You can close this tab and go back to your terminal.</p>
</body></html>
`

const loopbackAlreadyDonePage = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>Already finished</title></head>
<body style="font-family: system-ui, sans-serif; max-width: 32rem; margin: 4rem auto; padding: 0 1.5rem">
<h1>Already finished</h1>
<p>This sign-in is already complete. You can close this tab.</p>
</body></html>
`
