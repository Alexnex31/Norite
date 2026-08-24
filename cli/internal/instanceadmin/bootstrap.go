package instanceadmin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/Alexnex31/Norite/backend/operatortoken"
	"github.com/Alexnex31/Norite/cli/internal/apiclient"
	"github.com/Alexnex31/Norite/daemon/credentials"
)

// `norite instance bootstrap` — creating the account an instance is administered from.
//
// # Why this is a separate command from `norite instance init`
//
// The wizard writes a configuration file. At the moment it finishes, the backend has not been started and
// the database has not been migrated, so there is no instance to create an account on — the ordered steps
// are init, migrate, start, then this. Asking for an administrator's details during the wizard and holding
// them until a server appears would mean either keeping a password in memory across an unbounded wait, or
// writing it somewhere, and both are worse than a second command.
//
// docs/roadmap.md describes this as a step added *to* the wizard. It is a sibling instead, for the reason
// above, and ADR 0029 records the deviation.
//
// # Why the password is never a flag
//
// The same rule the wizard follows for the database password and `norite login` follows for its own: a
// flag value is visible in the process list to every other user on the machine, and in shell history
// afterwards. NORITE_ADMIN_PASSWORD is the scripted path.

// passwordEnvVar is the scripted source for the administrator's password.
const passwordEnvVar = "NORITE_ADMIN_PASSWORD"

// minSigningKeyLength mirrors auth.MinSigningKeyLength. Duplicated rather than imported because the
// backend's copy is under internal/ and unreachable from here — and unlike the token format, a number that
// drifts costs a confusing message rather than a broken credential: the instance enforces the real bound
// at startup and would refuse to run on a short key regardless.
const minSigningKeyLength = 32

// ErrNoTerminal is returned when an answer is needed and there is nowhere to ask for one.
var ErrNoTerminal = errors.New("this command needs an interactive terminal to ask its questions")

// Options is everything the command line supplies.
type Options struct {
	// ConfigPath overrides instance-configuration discovery. Empty means the documented order.
	ConfigPath string
	// Instance overrides the URL from the configuration, for an instance that does not know its own
	// address or is not reachable at it from here.
	Instance string

	Username    string
	Email       string
	DisplayName string
}

// Runner performs the bootstrap. The IO seams exist so the whole command is testable without a terminal.
type Runner struct {
	Options Options
	Out     io.Writer

	ReadLine   func(prompt string) (string, error)
	ReadSecret func(prompt string) (string, error)
	// Interactive reports whether the readers above are attached to a terminal.
	Interactive bool
	// JSON switches data-printing commands to their machine-readable form (CLAUDE.md rule 15). Bootstrap
	// ignores it: it is a conversation that ends in an account, not a command that prints data, the same
	// reason `norite instance init` has no JSON form.
	JSON bool

	// LoadConfig is indirected for tests; production leaves it nil and the real reader is used.
	LoadConfig func(path string) (Config, error)
	// NewClient is indirected for tests; production leaves it nil.
	NewClient func(baseURL string) *apiclient.Client
}

// bootstrapRequest matches the endpoint's payload. Field names are the contract's, not Go's.
type bootstrapRequest struct {
	Username    string `json:"username"`
	Email       string `json:"email"`
	Password    string `json:"password"`
	DisplayName string `json:"display_name,omitempty"`
}

// createdAccount is the part of the response worth showing.
type createdAccount struct {
	ID       string `json:"id"`
	Username string `json:"username"`
}

// Run creates the first administrator.
func (r *Runner) Run(ctx context.Context) error {
	cfg, err := r.loadConfig()
	if err != nil {
		return err
	}

	baseURL, err := r.resolveInstanceURL(cfg)
	if err != nil {
		return err
	}

	// Minted before anything is asked for, so a configuration that cannot produce one fails before a
	// password is typed rather than after.
	token, err := operatorToken(cfg.JWTSecret)
	if err != nil {
		return err
	}

	username, err := r.resolveField("Username: ", r.Options.Username, "username", "--username")
	if err != nil {
		return err
	}
	email, err := r.resolveField("Email: ", r.Options.Email, "email address", "--email")
	if err != nil {
		return err
	}
	password, err := r.resolvePassword()
	if err != nil {
		return err
	}

	if apiclient.LooksLikeHTTP(baseURL) {
		// Said before the request, not after: the password is about to cross the network in the clear and
		// the person can still stop. The same warning `norite login` gives, for the same reason.
		r.printf("Warning: %s is not HTTPS. The password will be sent unencrypted.\n", baseURL)
	}

	client := r.client(baseURL)
	var account createdAccount
	err = client.Do(ctx, "POST", "/api/v1/instance/bootstrap", token, bootstrapRequest{
		Username:    username,
		Email:       email,
		Password:    password,
		DisplayName: r.Options.DisplayName,
	}, &account)
	if err != nil {
		return r.explain(err, cfg)
	}

	// Sanitized on the way out of the response, so the value is safe wherever it goes afterwards — the
	// boundary rule M7 established (CLAUDE.md rule 19). This one is a name the instance echoed back.
	r.printf("Created %s, the administrator of %s.\n", apiclient.ForDisplay(account.Username), baseURL)
	r.printf("\nSign in with:\n  norite login --instance %s\n", baseURL)
	return nil
}

// operatorToken mints the credential that authorizes this command.
//
// The claim shape comes from the backend package that verifies it rather than being rebuilt here. Two
// implementations of one token format drift, and the failure mode is a bootstrap reporting a bad signature
// against a token that is perfectly well formed — see auth/operator.go's header.
func operatorToken(signingKey string) (string, error) {
	// Checked here rather than left to the signature failing at the instance. A hand-edited config with a
	// memorable key in it is the case worth naming, and the message says what is wrong without quoting any
	// part of the value (CLAUDE.md rule 8).
	//
	// The bound is the backend's own: HS256 is HMAC-SHA-256, whose security is capped by the key's
	// entropy, and 32 bytes is the hash output size — the point past which a longer key buys nothing.
	if len(signingKey) < minSigningKeyLength {
		return "", fmt.Errorf("this instance's signing key is shorter than %d bytes, so it cannot be "+
			"trusted — regenerate it with `norite instance init`", minSigningKeyLength)
	}
	return operatortoken.Issue([]byte(signingKey), time.Now())
}

// explain turns a failed request into advice, where there is any worth giving.
func (r *Runner) explain(err error, cfg Config) error {
	var apiErr *apiclient.Error
	if !errors.As(err, &apiErr) {
		return err
	}

	switch {
	case apiErr.Code == "already_bootstrapped":
		return fmt.Errorf("%w\n\nThis instance is already set up. Sign in with `norite login`, or use "+
			"`norite instance invite` to let somebody else create an account", err)

	case apiErr.Status == 401 && cfg.SecretFromEnv:
		// Worth distinguishing, because the two sources fail for opposite reasons and the wrong advice
		// sends somebody editing a file that is not being used.
		return fmt.Errorf("%w\n\nThe signing key came from NORITE_JWT_SECRET, not from %s. Check that the "+
			"running server was started with the same value", err, cfg.Path)

	case apiErr.Status == 401:
		return fmt.Errorf("%w\n\nThe instance did not accept this machine's signing key. Check that %s is "+
			"the configuration the running server was started with", err, cfg.Path)
	}
	return err
}

func (r *Runner) loadConfig() (Config, error) {
	if r.LoadConfig != nil {
		return r.LoadConfig(r.Options.ConfigPath)
	}
	return LoadConfig(r.Options.ConfigPath)
}

// resolveInstanceURL decides where to send the request.
//
// --instance wins over the configuration, because the address an instance publishes and the address it is
// reachable at from the operator's shell are often different — a server behind a proxy answers on
// localhost while public_base_url names the world-facing name.
func (r *Runner) resolveInstanceURL(cfg Config) (string, error) {
	raw := strings.TrimSpace(r.Options.Instance)
	if raw == "" {
		raw = cfg.PublicBaseURL
	}
	if raw == "" {
		return "", fmt.Errorf("this instance's configuration does not say where it is reachable "+
			"([http].public_base_url in %s); pass --instance", cfg.Path)
	}
	// The same validator `norite login` uses. This value becomes a request target, so it is refused rather
	// than sanitized when it cannot be one (M7).
	return credentials.ParseInstanceURL(raw)
}

// resolveField reads one required non-secret answer.
func (r *Runner) resolveField(prompt, preset, what, flag string) (string, error) {
	if value := strings.TrimSpace(preset); value != "" {
		return value, nil
	}
	if !r.Interactive {
		return "", fmt.Errorf("%w: pass %s, or run it from a terminal", ErrNoTerminal, flag)
	}

	value, err := r.ReadLine(prompt)
	if err != nil {
		return "", err
	}
	if value = strings.TrimSpace(value); value == "" {
		return "", fmt.Errorf("an %s is required", what)
	}
	return value, nil
}

func (r *Runner) resolvePassword() (string, error) {
	// The environment first, so a scripted run never depends on a terminal being present.
	if password := os.Getenv(passwordEnvVar); password != "" {
		return password, nil
	}
	if !r.Interactive {
		return "", fmt.Errorf("%w: set %s, or run it from a terminal", ErrNoTerminal, passwordEnvVar)
	}

	password, err := r.ReadSecret("Password: ")
	if err != nil {
		return "", err
	}
	if password == "" {
		// Refused locally rather than sent, so an empty answer does not come back as the instance's
		// deliberately vague refusal — which would read as "wrong password" (M7).
		return "", errors.New("a password is required")
	}

	// Asked twice, unlike `norite login`. There is nothing to check it against and no way to recover it:
	// a typo here creates an administrator nobody can sign in as, on an instance whose bootstrap endpoint
	// has just disabled itself. Password reset needs a mail relay this instance may not have.
	again, err := r.ReadSecret("Password (again): ")
	if err != nil {
		return "", err
	}
	if again != password {
		return "", errors.New("the two passwords did not match")
	}
	return password, nil
}

func (r *Runner) client(baseURL string) *apiclient.Client {
	if r.NewClient != nil {
		return r.NewClient(baseURL)
	}
	return apiclient.New(baseURL)
}

// printf writes to the output stream. The write error is dropped once, here, rather than at each call
// site — the justification the wizard's prompter documents, and what keeps errcheck satisfied.
func (r *Runner) printf(format string, args ...any) {
	_, _ = fmt.Fprintf(r.Out, format, args...)
}
