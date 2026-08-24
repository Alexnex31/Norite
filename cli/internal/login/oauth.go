package login

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/Alexnex31/Norite/cli/internal/termsafe"
)

// `norite login --provider google`, the loopback flow (Milestone M8).
//
// Four steps, and the ordering of the first two is load-bearing: bind the listener, mint the binding, open
// the browser, redeem what comes back. The listener is bound *first* because its address goes into the URL
// the browser is sent to, and a URL naming a port nothing is listening on is a sign-in that fails after the
// person has already consented.

// oauthProviders are the providers this client knows how to name.
//
// Checked locally so a typo fails at the terminal instead of as a 404 page in a browser, after a listener
// has been bound and a request made. The instance remains the authority on which of them it actually
// offers — this list is about spelling, not about configuration.
var oauthProviders = []string{"github", "google"}

// oauthProviderLabels are the names to print. A table rather than capitalizing the first letter, because
// that rule produces "Github", which is wrong, and no rule produces "GitHub" — the backend's verification
// page carries the same table for the same reason.
var oauthProviderLabels = map[string]string{
	"github": "GitHub",
	"google": "Google",
}

// providerLabel names a provider for a person, falling back to whatever was asked for.
func providerLabel(provider string) string {
	if label, ok := oauthProviderLabels[provider]; ok {
		return label
	}
	return provider
}

// signInWithOAuth runs the browser flow and returns the pair it produced.
func (r *Runner) signInWithOAuth(ctx context.Context, s session, provider string) (tokenPair, error) {
	listener, err := listenLoopback(r.ports())
	if err != nil {
		return tokenPair{}, err
	}
	defer func() { _ = listener.Close() }()

	verifier, challenge, err := mintFlowBinding()
	if err != nil {
		return tokenPair{}, err
	}

	target := authorizeURL(s.instanceURL, provider, challenge, listener.redirectURI())

	// Nobody is watching, and nothing opened. Waiting the full timeout here would be the worst of both:
	// a cron job that hangs for fifteen minutes and then reports a timeout, when the answer was knowable
	// at once. A non-interactive run with a *working* browser is a real case — a script launched from a
	// desktop session — so only the combination fails, and it names the flow that will cover it.
	failFast := !r.Interactive && !r.Options.NoBrowser

	// Printed whether or not the browser opens, and that is deliberate rather than defensive: a browser
	// that opens the wrong profile is the ordinary case, not the rare one, and this line is what rescues
	// it. Safe to print — see authorizeURL.
	if r.Options.NoBrowser {
		r.printf("Open this in a browser to sign in:\n\n  %s\n\n", target)
	} else {
		r.printf("Opening your browser to sign in. If it does not open, go to:\n\n  %s\n\n", target)
		if err := r.launchBrowser(ctx, target); err != nil {
			if failFast {
				return tokenPair{}, fmt.Errorf(
					"could not open a browser (%s), and there is no terminal for anyone to read the "+
						"sign-in link from; use --device-code, which needs neither",
					termsafe.Text(err.Error()))
			}
			// Sanitized because it can carry an OS message and a path, and because it is printed rather
			// than returned — so main's errorText backstop never sees it (rule 19).
			r.printf("Could not open a browser (%s); use the link above.\n\n", termsafe.Text(err.Error()))
		}
	}
	r.printf("Waiting on %s. Press Ctrl-C to stop.\n", listener.redirectURI())

	result, err := listener.wait(ctx)
	if err != nil {
		return tokenPair{}, err
	}
	if result.code == "" {
		return tokenPair{}, oauthFailure(result.failure)
	}

	return s.api.exchangeOAuthCode(ctx, oauthExchangeRequest{
		Code:         result.code,
		FlowVerifier: verifier,
		DeviceID:     s.deviceID,
		DeviceName:   s.deviceName,
	})
}

// checkProvider refuses a name this client does not know, before any flow is chosen.
//
// Checked locally so a typo fails at the terminal rather than as a 404 page in a browser, after a listener
// has been bound and a request made — and now also rather than as a device-code sign-in that quietly
// ignores the flag. The instance remains the authority on which providers it actually offers; this is
// about spelling.
func checkProvider(provider string) error {
	if provider == "" || slices.Contains(oauthProviders, provider) {
		return nil
	}
	return fmt.Errorf(
		"unknown sign-in provider %q: this client knows %s, and the instance decides which it offers",
		termsafe.Text(provider), strings.Join(oauthProviders, " and "))
}

// launchBrowser opens a URL. Indirected for tests; production leaves the field nil.
func (r *Runner) launchBrowser(ctx context.Context, target string) error {
	if r.openBrowser != nil {
		return r.openBrowser(ctx, target)
	}
	return openBrowser(ctx, target)
}

// ports is the list to bind, overridable so tests never contend for a fixed port on a machine running
// several packages at once.
func (r *Runner) ports() []int {
	if len(r.loopbackPorts) > 0 {
		return r.loopbackPorts
	}
	return loopbackPorts
}

// oauthFailure turns a code from the instance's vocabulary into something worth reading.
//
// The wording is this program's own. What arrives is a short code precisely so that no sentence written by
// a provider — or by whatever else can reach a loopback socket — is ever printed, and writing the prose
// here is the other half of that bargain.
func oauthFailure(code string) error {
	switch code {
	case "access_denied":
		return errors.New("the sign-in was canceled at the provider")
	case "email_unverified":
		return errors.New(
			"the provider has not verified that account's email address, so it cannot be used to sign in " +
				"here; verify it with the provider, or sign in with a password and link the provider from " +
				"settings")
	case "identity_linked_elsewhere":
		return errors.New("that provider account is already linked to a different Norite account")
	case "account_already_linked":
		return errors.New("that account is already linked to a different account at this provider")
	case "registration_closed":
		return errors.New("this instance requires an invite code to create an account")
	case "email_taken":
		return errors.New(
			"that provider account's email address is already registered here; sign in with a password " +
				"instead, and link the provider from settings")
	case "no_email":
		return errors.New("the provider did not share an email address, which this instance needs")
	case "malformed_code":
		return errors.New("something answered the sign-in listener with a code this instance did not issue")
	case "no_result":
		return errors.New("the browser came back without a result; run `norite login` again")
	default:
		// Including "server_error" and anything unrecognized. reduceFailure has already bounded and
		// stripped it, so printing it is safe; it is shown because an operator reading a support message
		// needs the instance's own word for what went wrong.
		return fmt.Errorf("the instance could not complete the sign-in (%s)", code)
	}
}
