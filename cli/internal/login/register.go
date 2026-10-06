// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package login

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/urfave/cli/v3"

	"github.com/Alexnex31/Norite/cli/internal/apiclient"
	"github.com/Alexnex31/Norite/daemon/credentials"
	"github.com/Alexnex31/Norite/daemon/termsafe"
)

// `norite register`, M20a's: an account from the command line. Without it the second person in M20a's
// journey — the one who redeems the invite — could only come into existence through curl or a provider the
// instance may not have configured, and a first client two people can share has to let the second one
// exist.
//
// It talks to the instance directly, as `norite login` and `norite instance bootstrap` do, and for the same
// reason: there is no account yet, so there is no daemon session to relay through. It stores nothing. An
// account is not signed in by registering (the endpoint deliberately does not log in), so the last thing it
// prints is the `norite login` that does.

// RegisterOptions is what `norite register` was asked to do.
type RegisterOptions struct {
	// Instance is the URL given on the command line; empty falls back as `norite login` does.
	Instance string
	// Username, Email: empty means ask.
	Username, Email string
	// DisplayName is optional; the instance defaults it to the username.
	DisplayName string
	// InviteCode is what an invite-only instance requires, and an open one ignores.
	InviteCode string
}

// Registrar performs a registration. Its dependencies are injected for the tests, as Runner's are.
type Registrar struct {
	Options RegisterOptions

	// Previous is the last login's record, for the instance it names. Zero when there is none.
	Previous credentials.Record

	Out         io.Writer
	ReadLine    func(prompt string) (string, error)
	ReadSecret  func(prompt string) (string, error)
	Interactive bool

	// newClient builds the API client. Indirected for tests; production leaves it nil.
	newClient func(baseURL string) *client
}

type registerRequest struct {
	Username    string `json:"username"`
	Email       string `json:"email"`
	Password    string `json:"password"`
	DisplayName string `json:"display_name,omitempty"`
	InviteCode  string `json:"invite_code,omitempty"`
}

type registrationAccepted struct {
	Message string `json:"message"`
}

// Run registers the account and says what comes next.
func (r *Registrar) Run(ctx context.Context) error {
	instanceURL, err := resolveInstanceURL(r.Options.Instance, r.Previous)
	if err != nil {
		return err
	}
	// Before anything is typed, as `norite login` warns: the point is to let somebody stop.
	if apiclient.LooksLikeHTTP(instanceURL) {
		r.printf("Warning: %s is plain HTTP, so the password you choose crosses the network unencrypted.\n",
			instanceURL)
	}

	username, err := r.ask(r.Options.Username, "Username: ", "--username")
	if err != nil {
		return err
	}
	email, err := r.ask(r.Options.Email, "Email: ", "--email")
	if err != nil {
		return err
	}
	password, err := r.newPassword()
	if err != nil {
		return err
	}

	var accepted registrationAccepted
	api := r.client(instanceURL)
	_, err = api.DoStatus(ctx, http.MethodPost, "/api/v1/auth/register", "", registerRequest{
		Username:    username,
		Email:       email,
		Password:    password,
		DisplayName: strings.TrimSpace(r.Options.DisplayName),
		InviteCode:  strings.TrimSpace(r.Options.InviteCode),
	}, &accepted)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Code == "invite_required" {
			// The one refusal worth rewording: the instance's own sentence cannot know which flag this
			// client takes the code in, or that the instance's operator is who makes one.
			return errors.New("this instance takes new accounts by invite: pass --invite-code CODE, " +
				"which its administrator makes with `norite instance invite create`")
		}
		return err
	}

	// The instance's sentence, which says either to check the mail or that the account is ready (it has no
	// mail relay to verify an address with). It is a stranger's text, so it passes termsafe (rule 19), and it
	// is printed as given rather than branched on: the contract says it is prose, not a signal.
	r.printf("%s\n", termsafe.Text(accepted.Message))
	r.printf("\nSign in with:\n  norite login --instance %s --email %s\n", instanceURL, termsafe.Text(email))
	return nil
}

// ask returns a flag's value, or asks for it, or says which flag would have answered.
func (r *Registrar) ask(given, prompt, flag string) (string, error) {
	if v := strings.TrimSpace(given); v != "" {
		return v, nil
	}
	if !r.Interactive {
		return "", fmt.Errorf("%w: pass %s, or run it from a terminal", ErrNoTerminal, flag)
	}
	v, err := r.ReadLine(prompt)
	if err != nil {
		return "", err
	}
	if v = strings.TrimSpace(v); v == "" {
		return "", fmt.Errorf("%s is required", strings.TrimPrefix(flag, "--"))
	}
	return v, nil
}

// newPassword reads the password to give the new account: from NORITE_PASSWORD for a script, as `norite
// login` reads one, or typed twice without echo. Twice because nothing else would catch a typo — the
// account would exist with a password nobody knows, and the only way back is a reset that needs a mail
// relay the instance may not have. The length rule is the instance's to enforce and to explain.
func (r *Registrar) newPassword() (string, error) {
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
		return "", errors.New("a password is required")
	}
	again, err := r.ReadSecret("Password, again: ")
	if err != nil {
		return "", err
	}
	if again != password {
		return "", errors.New("the two passwords differ; nothing was sent")
	}
	return password, nil
}

func (r *Registrar) client(baseURL string) *client {
	if r.newClient != nil {
		return r.newClient(baseURL)
	}
	return newClient(baseURL)
}

func (r *Registrar) printf(format string, args ...any) {
	_, _ = fmt.Fprintf(r.Out, format, args...)
}

// RegisterCommand builds `norite register`.
func RegisterCommand() *cli.Command {
	return &cli.Command{
		Name:  "register",
		Usage: "Create an account on a Norite instance",
		Description: "Creates an account, then says how to sign in to it with `norite login`: registering\n" +
			"does not sign in.\n\n" +
			"The password is read twice without echo and is never accepted as a flag — a flag value\n" +
			"is visible in the process list to every other user on the machine. For scripted use, set\n" +
			passwordEnvVar + " instead.\n\n" +
			"An instance with a mail relay sends a link to confirm the address before the account can\n" +
			"be used; one without creates it ready. Either way the instance answers the same whether\n" +
			"or not the address already has an account, so this cannot tell you which.",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  "instance",
				Usage: "`URL` of the instance (default: " + instanceEnvVar + ", then the one from the last login)",
			},
			&cli.StringFlag{Name: "username", Usage: "the account's `USERNAME`, its public @handle (default: ask)"},
			&cli.StringFlag{Name: "email", Usage: "the account's `EMAIL` address (default: ask)"},
			&cli.StringFlag{Name: "display-name", Usage: "the `NAME` people see (default: the username)"},
			&cli.StringFlag{
				Name:  "invite-code",
				Usage: "the `CODE` an invite-only instance requires; an open one ignores it",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			var previous credentials.Record
			if store, err := credentials.Open(); err == nil {
				// Only for the instance it names. A record that cannot be read is no reason to refuse:
				// --instance answers the question anyway, and the error says so when nothing does.
				previous, _ = store.LoadRecord()
			}
			readLine, readSecret, interactive := terminalReaders(os.Stdin, cmd.Writer)
			r := &Registrar{
				Options: RegisterOptions{
					Instance:    cmd.String("instance"),
					Username:    cmd.String("username"),
					Email:       cmd.String("email"),
					DisplayName: cmd.String("display-name"),
					InviteCode:  cmd.String("invite-code"),
				},
				Previous:    previous,
				Out:         cmd.Writer,
				ReadLine:    readLine,
				ReadSecret:  readSecret,
				Interactive: interactive,
			}
			if err := r.Run(ctx); err != nil {
				// As `norite login` returns it: main recognizes ErrNoTerminal and exits 2 without the prefix.
				if errors.Is(err, ErrNoTerminal) {
					return err
				}
				return cli.Exit(err.Error(), 1)
			}
			return nil
		},
	}
}
