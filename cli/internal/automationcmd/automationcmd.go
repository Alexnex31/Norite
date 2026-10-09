// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package automationcmd is `norite automation`: turning the daemon's port for scripts on and off, and the
// two commands a script uses to reach it (M22).
//
// The port is the lower of the daemon's two local tiers (rule 16, ADR 0017): TCP on 127.0.0.1, behind a
// secret the daemon mints for each run, carrying requests made with an API token the script holds. This
// package is where a person meets it, and it handles both secrets:
//
//   - The port secret is never printed, in text or in JSON, and never taken from a flag. `run` puts it in
//     the environment of the one program it starts; `request` reads it from there or from the daemon's
//     file.
//   - The API token is read from NORITE_API_TOKEN and nowhere else. A flag's value is in the process list
//     and the shell's history.
//
// enable, disable and status ask the daemon over the attach socket, as `config split` does. run and
// request never touch that socket: they are what a script uses, and a script is not a first-party client.
package automationcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/urfave/cli/v3"

	"github.com/Alexnex31/Norite/cli/internal/clierr"
	"github.com/Alexnex31/Norite/cli/internal/daemonclient"
	"github.com/Alexnex31/Norite/cli/internal/output"
	"github.com/Alexnex31/Norite/daemon/ipc"
)

// The environment a script finds the port in, and its token.
const (
	// EnvAddress and EnvSecret are set by `run` for the program it starts, and read by `request`.
	EnvAddress = "NORITE_AUTOMATION_ADDRESS"
	EnvSecret  = "NORITE_AUTOMATION_SECRET"
	// EnvToken is the script's API token, which only its owner sets.
	EnvToken = "NORITE_API_TOKEN"
)

// Connector attaches to the daemon for one command, returning a caller and a function that detaches.
type Connector func(ctx context.Context) (daemonclient.Caller, func(), error)

// Command is the `norite automation` group.
func Command(connect Connector) *cli.Command {
	return &cli.Command{
		Name:  "automation",
		Usage: "Let scripts on this machine act through the daemon, with a token you scope",
		Description: "The daemon can open a port on this machine for scripts and bots. It is closed until you\n" +
			"turn it on. A script needs two things to use it: the port's own secret, which `run` hands\n" +
			"it, and an API token from `norite token create`, which decides what it may do. Messages a\n" +
			"token sends are marked as automated for everyone who reads them.\n\n" +
			"   norite token create --name bot --scope messages.write --out bot.token\n" +
			"   norite automation enable\n" +
			"   NORITE_API_TOKEN=$(cat bot.token) norite automation run -- ./bot.sh\n\n" +
			"and inside bot.sh:\n\n" +
			"   norite automation request POST /channels/<id>/messages --body '{\"content\":\"hello\"}'",
		Commands: []*cli.Command{
			{
				Name:  "enable",
				Usage: "Open the port, for the instance you are signed in to",
				Description: "The port serves the instance the daemon is signed in to when you run this, and no\n" +
					"other: after signing in somewhere else, run it again. Running it again also replaces\n" +
					"the port's secret. The daemon must be running.",
				Flags: []cli.Flag{&cli.IntFlag{Name: "port",
					Usage: fmt.Sprintf("the `PORT` on 127.0.0.1 to listen on; %d unless you chose another before", ipc.DefaultAutomationPort)}},
				Action: run(connect, enable),
			},
			{
				Name:  "disable",
				Usage: "Close the port and keep it closed",
				Action: run(connect, func(ctx context.Context, _ *cli.Command, c daemonclient.Caller) (output.Result, error) {
					var st ipc.AutomationStatus
					if err := daemonclient.Local(ctx, c, ipc.PathAutomationDisable, &st); err != nil {
						return nil, err
					}
					return statusView(st), nil
				}),
			},
			{
				Name:  "status",
				Usage: "Say whether the port is on, where, and for which instance",
				Action: run(connect, func(ctx context.Context, _ *cli.Command, c daemonclient.Caller) (output.Result, error) {
					var st ipc.AutomationStatus
					if err := daemonclient.LocalRead(ctx, c, ipc.PathAutomation, &st); err != nil {
						return nil, err
					}
					return statusView(st), nil
				}),
			},
			{
				Name:      "run",
				Usage:     "Start a program with the port's address and secret in its environment",
				ArgsUsage: "-- <command> [arguments...]",
				Description: "Sets " + EnvAddress + " and " + EnvSecret + " for the program and nothing else:\n" +
					"neither is printed, and neither is on a command line. The program brings its own\n" +
					EnvToken + ". It takes this command's place, so its exit code and its signals are\n" +
					"its own.",
				Action: runProgram,
			},
			{
				Name:      "request",
				Usage:     "Make one request through the port, as a script",
				ArgsUsage: "<method> <path>",
				Description: "The path is the REST API's, under /api/v1, such as /channels/<id>/messages. The token\n" +
					"is read from " + EnvToken + ", and the port from the environment `run` sets or, outside\n" +
					"`run`, from the daemon's own file. Prints the instance's JSON answer.\n\n" +
					"Exits 0 when the instance answered 2xx; 4 when it or the daemon refused the request;\n" +
					"3 when the port is not open, the token was not accepted, or a rate limit was hit; and\n" +
					"1 when the instance failed.",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "body", Usage: "the `JSON` to send"},
					&cli.StringFlag{Name: "body-file", Usage: "read the JSON to send from this `FILE`, or - for standard input"},
				},
				Action: request,
			},
		},
	}
}

// run attaches to the daemon, runs a verb that asks it something, and renders the answer.
func run(connect Connector, v func(ctx context.Context, cmd *cli.Command, c daemonclient.Caller) (output.Result, error)) cli.ActionFunc {
	return func(ctx context.Context, cmd *cli.Command) error {
		if cmd.Args().Len() != 0 {
			return clierr.Usage("norite automation %s takes no arguments", cmd.Name)
		}
		// Checked before attaching, so a mistake is a usage error on a machine with no daemon running.
		if cmd.IsSet("port") {
			if p := cmd.Int("port"); p < 1 || p > 65535 {
				return clierr.Usage("--port must be between 1 and 65535")
			}
		}
		if connect == nil {
			return errors.New("this build has no way to reach the daemon")
		}
		c, detach, err := connect(ctx)
		if err != nil {
			return err
		}
		defer detach()
		result, err := v(ctx, cmd, c)
		if err != nil {
			return err
		}
		return output.Render(writer(cmd), cmd.Root().Bool("json"), result)
	}
}

func writer(cmd *cli.Command) io.Writer {
	if w := cmd.Root().Writer; w != nil {
		return w
	}
	return os.Stdout
}

// enable asks how the port stands, then asks for it to be opened.
//
// The first request is not only for the port number. A request to the daemon carries nothing of the user's
// in its path, because a daemon from before such requests existed would relay the path to its instance;
// the port number is the one exception, and it is sent only once the daemon has shown, by answering a
// request that says nothing, that it knows what these are.
func enable(ctx context.Context, cmd *cli.Command, c daemonclient.Caller) (output.Result, error) {
	var st ipc.AutomationStatus
	if err := daemonclient.LocalRead(ctx, c, ipc.PathAutomation, &st); err != nil {
		return nil, err
	}
	port := st.Port
	if cmd.IsSet("port") {
		port = int(cmd.Int("port"))
	}
	if port < 1 || port > 65535 {
		port = ipc.DefaultAutomationPort
	}
	if err := daemonclient.Local(ctx, c, ipc.PathAutomationEnable(port), &st); err != nil {
		return nil, err
	}
	return statusView(st), nil
}

// statusView is how the port stands, as these commands print it. It has no field for the secret.
type statusView struct {
	Enabled  bool   `json:"enabled"`
	Port     int    `json:"port"`
	Instance string `json:"instance"`
	Open     bool   `json:"open"`
	Address  string `json:"address"`
	Problem  string `json:"problem"`
}

// Text draws it for a person. Every string is the daemon's, and the instance's name came from a file a
// person can edit, so each passes termsafe (rule 19).
func (v statusView) Text(t *output.Text) {
	switch {
	case !v.Enabled:
		t.Line("The automation port is off. `norite automation enable` opens it on port %d.", v.Port)
	case v.Open && v.Problem != "":
		t.Line("The automation port is on, at %s, for %s.", output.Clean(v.Address), output.Clean(v.Instance))
		t.Line("It is refusing requests: %s.", output.Clean(v.Problem))
	case v.Open:
		t.Line("The automation port is on, at %s, for %s.", output.Clean(v.Address), output.Clean(v.Instance))
		t.Line("Start a script with `norite automation run -- <command>`, with %s set to its token.", EnvToken)
	default:
		t.Line("The automation port is on and not open: %s.", output.Clean(v.Problem))
	}
}

// ---------- run ----------

// replace starts a program in this one's place. A variable so a test can see what would have been started.
var replace = replaceProcess

// located finds where the port is: the daemon's file. A variable for the tests.
var located = ipc.ReadAutomationFile

func portOff(err error) error {
	if errors.Is(err, ipc.ErrAutomationOff) {
		return clierr.Unavailable("the automation port is not open; turn it on with `norite automation enable`, " +
			"with the daemon running")
	}
	return clierr.Unavailable("%s", output.Clean(err.Error()))
}

func runProgram(_ context.Context, cmd *cli.Command) error {
	argv := cmd.Args().Slice()
	if len(argv) == 0 {
		return clierr.Usage("norite automation run takes the program to start: norite automation run -- <command>")
	}
	file, err := located()
	if err != nil {
		return portOff(err)
	}
	path, err := exec.LookPath(argv[0])
	if err != nil {
		return clierr.Usage("cannot start %s: %s", output.Clean(argv[0]), output.Clean(reason(err)))
	}
	return replace(path, argv, withPort(os.Environ(), file))
}

// withPort is env with the port's two variables set, replacing any already there: a program started from
// inside another `run`, after the daemon restarted, must not be handed the last run's secret.
func withPort(env []string, file ipc.AutomationFile) []string {
	out := make([]string, 0, len(env)+2)
	for _, kv := range env {
		if strings.HasPrefix(kv, EnvAddress+"=") || strings.HasPrefix(kv, EnvSecret+"=") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, EnvAddress+"="+file.Address, EnvSecret+"="+file.Secret)
}

func reason(err error) string {
	var ee *exec.Error
	if errors.As(err, &ee) {
		return ee.Err.Error()
	}
	return err.Error()
}

// ---------- request ----------

// maxBody bounds what `request` sends, under the port's own bound on a frame.
const maxBody = 512 << 10

// requestTimeout bounds one request, connecting included.
const requestTimeout = 2 * time.Minute

func request(ctx context.Context, cmd *cli.Command) error {
	if cmd.Args().Len() != 2 {
		return clierr.Usage("usage: norite automation request <method> <path>")
	}
	method, path := strings.ToUpper(cmd.Args().Get(0)), cmd.Args().Get(1)
	if !ipc.ValidMethod(method) {
		return clierr.Usage("the method must be GET, POST, PUT, PATCH or DELETE")
	}
	if !strings.HasPrefix(path, "/") || len(path) > 2048 {
		return clierr.Usage("the path is the REST API's and starts with /, such as /users/@me")
	}
	body, err := bodyOf(cmd)
	if err != nil {
		return err
	}
	token := os.Getenv(EnvToken)
	if token == "" {
		return clierr.Usage("set %s to the script's API token, from `norite token create`. It is read from "+
			"the environment and never from a flag", EnvToken)
	}
	if !ipc.LooksLikeAPIToken(token) {
		return clierr.Usage("%s does not hold an API token: one begins nat_ and comes from `norite token create`", EnvToken)
	}

	// Inside `run`, the environment says where the port is. Outside it, the daemon's file does. Both
	// variables or neither: half of one run's pair with half of another's opens nothing.
	file := ipc.AutomationFile{Address: os.Getenv(EnvAddress), Secret: os.Getenv(EnvSecret)}
	if file.Address == "" || file.Secret == "" {
		if file, err = located(); err != nil {
			return portOff(err)
		}
	}

	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	c, err := ipc.DialAutomation(ctx, file, token)
	if err != nil {
		return dialFailure(err)
	}
	defer func() { _ = c.Close() }()
	resp, err := c.Do(ctx, method, path, body)
	if err != nil {
		var ce *ipc.CloseError
		if errors.As(err, &ce) {
			return clierr.Unavailable("the automation port closed the connection: %s", output.Clean(ce.Reason))
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return clierr.Unavailable("no answer within %s; the request may or may not have reached the instance", requestTimeout)
		}
		return clierr.Unavailable("%s", output.Clean(err.Error()))
	}
	return outcome(writer(cmd), resp)
}

func bodyOf(cmd *cli.Command) (json.RawMessage, error) {
	inline, file := cmd.String("body"), cmd.String("body-file")
	var raw []byte
	switch {
	case inline != "" && file != "":
		return nil, clierr.Usage("--body and --body-file are two ways to say one thing; give one")
	case inline != "":
		raw = []byte(inline)
	case file == "-":
		in := cmd.Root().Reader
		if in == nil {
			in = os.Stdin
		}
		data, err := io.ReadAll(io.LimitReader(in, maxBody+1))
		if err != nil {
			return nil, fmt.Errorf("reading the body from standard input: %w", err)
		}
		raw = data
	case file != "":
		info, err := os.Stat(file)
		if err != nil || !info.Mode().IsRegular() {
			return nil, clierr.Usage("--body-file %s is not a file that can be read", output.Clean(file))
		}
		if info.Size() > maxBody {
			return nil, clierr.Usage("--body-file is larger than %d KiB", maxBody>>10)
		}
		data, err := os.ReadFile(file) //nolint:gosec // the file the caller named
		if err != nil {
			return nil, clierr.Usage("--body-file %s is not a file that can be read", output.Clean(file))
		}
		raw = data
	default:
		return nil, nil
	}
	if len(raw) > maxBody {
		return nil, clierr.Usage("the body is larger than %d KiB", maxBody>>10)
	}
	raw = bytes.TrimSpace(raw)
	if !json.Valid(raw) {
		return nil, clierr.Usage("the body is not JSON")
	}
	return raw, nil
}

func dialFailure(err error) error {
	var ce *ipc.CloseError
	switch {
	case errors.Is(err, ipc.ErrAutomationOff):
		return portOff(err)
	case errors.As(err, &ce) && ce.Code == ipc.CloseAutomationRefused:
		return clierr.Unavailable("%s. The secret changes whenever the daemon starts or the port is turned on "+
			"again; start the script again with `norite automation run`", output.Clean(ce.Reason))
	case errors.As(err, &ce):
		return clierr.Unavailable("the automation port refused the connection: %s", output.Clean(ce.Reason))
	}
	return clierr.Unavailable("%s", output.Clean(err.Error()))
}

// outcome prints the instance's answer and decides the exit code, as daemonclient.Call does for a verb:
// what was refused is 4, what may work on another try is 3, and a failure of the instance is 1. The
// differences are in the wording, because the credential here is the script's token and not a sign-in.
func outcome(w io.Writer, resp ipc.Response) error {
	if resp.Error != nil {
		msg := output.Clean(resp.Error.Message)
		switch resp.Error.Code {
		case ipc.RelayRefused, ipc.RelayBadRequest:
			return &clierr.RefusedError{Message: msg}
		case ipc.RelayTooLarge:
			return errors.New(msg)
		}
		return clierr.Unavailable("%s", msg)
	}
	if resp.Status == nil {
		return errors.New("the automation port answered with neither a status nor a reason")
	}
	status := *resp.Status
	empty := len(bytes.TrimSpace(resp.Body)) == 0 || string(bytes.TrimSpace(resp.Body)) == "null"

	if status < 200 || status >= 300 {
		// Decided where every verb's is, so a script's request and a verb cannot come to disagree about
		// what an answer means. Only the 401's wording is this command's: the credential is the token.
		return daemonclient.Refusal(status, resp.Body, "the instance did not accept the token in "+EnvToken+
			": it was revoked, or was made on another instance. `norite token list` shows what is live")
	}
	if empty {
		if status == http.StatusNoContent {
			return nil
		}
		// The daemon drops a body that is not JSON. A 2xx with none, where the API always sends one, is
		// something in front of the instance answering for it, and is not success (M22 /code-review).
		return fmt.Errorf("the instance answered HTTP %d with no JSON body, which this API never does: "+
			"something between the daemon and the instance may be answering for it", status)
	}
	// The instance's body, exactly, with what a terminal would act on escaped. A parser reads the same
	// values either way. Numbers are kept as written: decoded the ordinary way they become float64, and
	// an integer above 2^53 would be printed as a different one.
	var v any
	dec := json.NewDecoder(bytes.NewReader(resp.Body))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return fmt.Errorf("the instance's answer does not decode: %w", err)
	}
	return output.WriteJSON(w, v)
}
