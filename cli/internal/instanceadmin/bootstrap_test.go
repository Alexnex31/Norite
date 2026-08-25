package instanceadmin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/operatortoken"
)

// testSigningKey stands in for the value `norite instance init` generates. Its length is what matters.
const testSigningKey = "an-instance-signing-key-of-at-least-32-bytes"

// writeConfig lays down a minimal instance.toml, the way the wizard would.
func writeConfig(t *testing.T, baseURL string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "instance.toml")
	body := "[http]\npublic_base_url = \"" + baseURL + "\"\n\n[auth]\njwt_secret = \"" + testSigningKey + "\"\n"
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

// fakeInstance answers the bootstrap endpoint and records what it was sent.
type fakeInstance struct {
	server *httptest.Server
	// bearer is the Authorization header of the last request, so a test can verify the token itself
	// rather than trusting that one was sent.
	bearer string
	body   map[string]string
	status int
	reply  string
}

func newFakeInstance(t *testing.T) *fakeInstance {
	t.Helper()
	f := &fakeInstance{status: http.StatusCreated, reply: `{"id":"1","username":"ada"}`}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.bearer = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		_ = json.NewDecoder(r.Body).Decode(&f.body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.status)
		_, _ = w.Write([]byte(f.reply))
	}))
	t.Cleanup(f.server.Close)
	return f
}

// newRunner wires a runner against a fake instance, with scripted answers.
func newRunner(t *testing.T, f *fakeInstance, opts Options, answers ...string) (*Runner, *strings.Builder) {
	t.Helper()

	if opts.ConfigPath == "" {
		opts.ConfigPath = writeConfig(t, f.server.URL)
	}
	out := &strings.Builder{}
	queue := append([]string{}, answers...)
	next := func(string) (string, error) {
		if len(queue) == 0 {
			return "", nil
		}
		answer := queue[0]
		queue = queue[1:]
		return answer, nil
	}

	return &Runner{
		Options:     opts,
		Out:         out,
		ReadLine:    next,
		ReadSecret:  next,
		Interactive: true,
	}, out
}

// The command's whole job, and the property that makes it work at all: the token it presents is one the
// instance's own signing key verifies.
func TestBootstrapPresentsAnOperatorTokenTheInstanceCanVerify(t *testing.T) {
	f := newFakeInstance(t)
	r, out := newRunner(t, f, Options{Username: "ada", Email: "ada@example.com"},
		"a-test-password", "a-test-password")

	require.NoError(t, r.Run(context.Background()))

	require.NotEmpty(t, f.bearer, "a bootstrap request must carry a credential")
	assert.NoError(t, operatortoken.Verify([]byte(testSigningKey), f.bearer, time.Now()),
		"the token must verify against the key in the config the command read")

	assert.Equal(t, "ada", f.body["username"])
	assert.Equal(t, "a-test-password", f.body["password"])
	assert.Contains(t, out.String(), "Created ada")
}

// The signing key is the one secret this command handles, and it must never reach the terminal — not in
// the success message, not in an error. CLAUDE.md rule 8.
func TestTheSigningKeyNeverReachesTheTerminal(t *testing.T) {
	f := newFakeInstance(t)
	f.status = http.StatusUnauthorized
	f.reply = `{"error":{"code":"unauthorized","message":"nope","request_id":"r1"}}`

	r, out := newRunner(t, f, Options{Username: "ada", Email: "ada@example.com"},
		"a-test-password", "a-test-password")
	err := r.Run(context.Background())

	require.Error(t, err)
	assert.NotContains(t, out.String(), testSigningKey)
	assert.NotContains(t, err.Error(), testSigningKey)
}

// A password typed once cannot be checked against anything, and a typo here creates an administrator
// nobody can sign in as — on an instance whose bootstrap endpoint has just disabled itself, and which may
// have no mail relay to reset through.
func TestAMistypedPasswordIsRefusedBeforeItIsSent(t *testing.T) {
	f := newFakeInstance(t)
	r, _ := newRunner(t, f, Options{Username: "ada", Email: "ada@example.com"},
		"a-test-password", "a-different-one")

	err := r.Run(context.Background())
	require.ErrorContains(t, err, "did not match")
	assert.Empty(t, f.body, "nothing may be sent when the two answers disagree")
}

// The scripted path, and the rule behind it: a password must never be a flag, because a flag value is
// visible in the process list to every other user on the machine.
func TestThePasswordComesFromTheEnvironmentWhenSet(t *testing.T) {
	t.Setenv(passwordEnvVar, "from-the-environment")

	f := newFakeInstance(t)
	// No scripted answers at all: if the command prompts, it reads empty and this fails.
	r, _ := newRunner(t, f, Options{Username: "ada", Email: "ada@example.com"})

	require.NoError(t, r.Run(context.Background()))
	assert.Equal(t, "from-the-environment", f.body["password"])
}

// Every question this command asks must be answerable without a terminal, and say which flag or variable
// answers it — the rule `norite login` follows, so a cron job or a Dockerfile is never stuck.
func TestEachQuestionSaysWhatWouldAnswerItWithoutATerminal(t *testing.T) {
	f := newFakeInstance(t)

	for _, tc := range []struct {
		name    string
		opts    Options
		mustSay string
	}{
		{"username", Options{}, "--username"},
		{"email", Options{Username: "ada"}, "--email"},
		{"password", Options{Username: "ada", Email: "ada@example.com"}, passwordEnvVar},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := newRunner(t, f, tc.opts)
			r.Interactive = false

			err := r.Run(context.Background())
			require.ErrorIs(t, err, ErrNoTerminal)
			assert.Contains(t, err.Error(), tc.mustSay)
		})
	}
}

// An instance that already has an administrator. The advice matters as much as the refusal: this is the
// error an operator hits when they do not realize setup already happened, and "sign in" is the answer.
func TestAnAlreadyBootstrappedInstanceSaysWhatToDoInstead(t *testing.T) {
	f := newFakeInstance(t)
	f.status = http.StatusConflict
	f.reply = `{"error":{"code":"already_bootstrapped",` +
		`"message":"this instance already has an administrator","request_id":"r1"}}`

	r, _ := newRunner(t, f, Options{Username: "ada", Email: "ada@example.com"},
		"a-test-password", "a-test-password")
	err := r.Run(context.Background())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "norite login")
}

// A signing key from the environment and one from the file fail for opposite reasons, and the wrong advice
// sends somebody editing a file that is not being used.
func TestARefusalNamesWhereTheKeyCameFrom(t *testing.T) {
	f := newFakeInstance(t)
	f.status = http.StatusUnauthorized
	f.reply = `{"error":{"code":"unauthorized","message":"nope","request_id":"r1"}}`

	t.Run("from the file", func(t *testing.T) {
		r, _ := newRunner(t, f, Options{Username: "ada", Email: "ada@example.com"},
			"a-test-password", "a-test-password")
		err := r.Run(context.Background())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "instance.toml")
	})

	t.Run("from the environment", func(t *testing.T) {
		t.Setenv("NORITE_JWT_SECRET", testSigningKey)
		r, _ := newRunner(t, f, Options{Username: "ada", Email: "ada@example.com"},
			"a-test-password", "a-test-password")
		err := r.Run(context.Background())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "NORITE_JWT_SECRET")
	})
}

// A hand-edited config with a memorable key in it. Caught here rather than at the instance, and the
// message says what is wrong without quoting any part of the value.
func TestAShortSigningKeyIsRefusedLocally(t *testing.T) {
	f := newFakeInstance(t)
	path := filepath.Join(t.TempDir(), "instance.toml")
	require.NoError(t, os.WriteFile(path,
		[]byte("[http]\npublic_base_url = \""+f.server.URL+"\"\n\n[auth]\njwt_secret = \"tiny\"\n"), 0o600))

	r, _ := newRunner(t, f, Options{ConfigPath: path, Username: "ada", Email: "ada@example.com"},
		"a-test-password", "a-test-password")

	err := r.Run(context.Background())
	require.ErrorContains(t, err, "signing key")
	assert.NotContains(t, err.Error(), "tiny", "the message must not quote the key")
	assert.Empty(t, f.bearer, "nothing may be sent when no usable token can be minted")
}

// The environment overrides the file, which docs/architecture.md §4 fixes and which is load-bearing here:
// the flagship injects NORITE_JWT_SECRET from a Kubernetes Secret, and a config file baked into an image
// must never shadow it — a token minted from a stale file key would be refused by the running server, and
// the error would point at the wrong thing entirely.
func TestTheEnvironmentSigningKeyOverridesTheFile(t *testing.T) {
	const injected = "a-completely-different-injected-signing-key"
	t.Setenv("NORITE_JWT_SECRET", injected)

	f := newFakeInstance(t)
	r, _ := newRunner(t, f, Options{Username: "ada", Email: "ada@example.com"},
		"a-test-password", "a-test-password")
	require.NoError(t, r.Run(context.Background()))

	assert.NoError(t, operatortoken.Verify([]byte(injected), f.bearer, time.Now()))
	assert.Error(t, operatortoken.Verify([]byte(testSigningKey), f.bearer, time.Now()),
		"the file's key must not be the one that signed it")
}

// The URL becomes a request target, so it is refused rather than sanitized when it cannot be one — the
// rule ParseInstanceURL follows (M7).
func TestAHostileInstanceURLIsRefused(t *testing.T) {
	f := newFakeInstance(t)
	r, _ := newRunner(t, f, Options{
		Instance: "https://example.com/" + "\x1b[2Kevil",
		Username: "ada", Email: "ada@example.com",
	}, "a-test-password", "a-test-password")

	err := r.Run(context.Background())
	require.Error(t, err)
	assert.Empty(t, f.bearer, "nothing may be sent to a URL that could not be parsed")
}

// A name the instance chose, on its way to a terminal. CLAUDE.md rule 19.
func TestAHostileUsernameInTheResponseIsSanitized(t *testing.T) {
	f := newFakeInstance(t)
	f.reply = `{"id":"1","username":"ada` + `\u001b[2Kroot"}`

	r, out := newRunner(t, f, Options{Username: "ada", Email: "ada@example.com"},
		"a-test-password", "a-test-password")
	require.NoError(t, r.Run(context.Background()))

	assert.NotContains(t, out.String(), "\x1b")
	assert.Contains(t, out.String(), "ada")
}

// An instance that does not know its own address is a supported configuration, so the command says which
// flag supplies it rather than failing with a URL parse error on an empty string.
func TestNoConfiguredAddressAsksForOne(t *testing.T) {
	path := filepath.Join(t.TempDir(), "instance.toml")
	require.NoError(t, os.WriteFile(path, []byte("[auth]\njwt_secret = \""+testSigningKey+"\"\n"), 0o600))

	r := &Runner{Options: Options{ConfigPath: path}, Out: &strings.Builder{}, Interactive: false}
	err := r.Run(context.Background())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "--instance")
}

// Warned before the request, not after: the password is about to cross the network in the clear and the
// person can still stop.
func TestAPlaintextInstanceIsWarnedAbout(t *testing.T) {
	f := newFakeInstance(t)
	r, out := newRunner(t, f, Options{Username: "ada", Email: "ada@example.com"},
		"a-test-password", "a-test-password")

	require.NoError(t, r.Run(context.Background()))
	assert.Contains(t, out.String(), "not HTTPS")
}
