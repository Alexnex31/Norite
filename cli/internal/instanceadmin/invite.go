package instanceadmin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/urfave/cli/v3"

	"github.com/Alexnex31/Norite/cli/internal/apiclient"
)

// `norite instance invite` — the codes that let somebody onto a gated instance.
//
// # Why this authenticates as the operator rather than as an administrator
//
// The endpoints behind it accept either an instance administrator's access token or an operator token.
// This command can only produce the second, and that is a consequence of where the CLI sits rather than a
// decision: an attach client does not hold its account's tokens — the daemon does (ADR 0011) — and the
// local IPC socket that would let the CLI ask the daemon to make an authenticated call is M19's. Until
// then, "run it where the config file is" is the only authority a command like this can present.
//
// The practical effect is that invites are managed from the machine running the instance, which is the
// same place `norite instance bootstrap` runs and a reasonable place for this to live in the meantime.

// inviteView is one invite as this command prints it, and the shape contracts/cli-json/ pins.
//
// Its own type rather than the API's response struct, deliberately: the wire format is the instance's, and
// what a client prints under --json is a contract this repository owns (CLAUDE.md rule 15). Reusing the
// API shape would make an instance's field rename a silent change to a scripted caller's input.
type inviteView struct {
	Code string `json:"code"`
	// CreatedBy is null for an invite the instance operator issued, who is not an account.
	CreatedBy *string `json:"created_by"`
	// MaxUses is null for an unlimited invite. Null rather than 0, because 0 would read as "no uses left".
	MaxUses   *int32     `json:"max_uses"`
	Uses      int32      `json:"uses"`
	ExpiresAt *time.Time `json:"expires_at"`
	CreatedAt time.Time  `json:"created_at"`
}

// InviteCommand builds `norite instance invite`.
func InviteCommand() *cli.Command {
	return &cli.Command{
		Name:  "invite",
		Usage: "Create and manage the invite codes that admit new accounts",
		Description: "Invite codes are what registration requires while this instance is set to\n" +
			"invite-only. On an open instance they are accepted and ignored, so a code handed out\n" +
			"before the policy changed keeps working afterwards.\n\n" +
			"Like `norite instance bootstrap`, these run against this machine's copy of the instance\n" +
			"configuration and need no login: the signing key in that file is what proves you\n" +
			"administer the instance.",
		Commands: []*cli.Command{
			inviteCreateCommand(),
			inviteListCommand(),
			inviteRevokeCommand(),
		},
	}
}

func inviteCreateCommand() *cli.Command {
	return &cli.Command{
		Name:  "create",
		Usage: "Mint an invite code",
		Description: "Prints a code to hand to whoever should be able to create an account.\n\n" +
			"With neither flag the code is unlimited and never expires, which is what a link in a\n" +
			"group chat wants. --max-uses and --expires-in narrow it.",
		Flags: append(configFlags(),
			&cli.IntFlag{
				Name:  "max-uses",
				Usage: "allow at most `N` accounts to be created with it (default: unlimited)",
			},
			&cli.DurationFlag{
				Name:  "expires-in",
				Usage: "expire it after `DURATION`, e.g. 168h or 30m (default: never)",
			},
		),
		Action: func(ctx context.Context, cmd *cli.Command) error {
			r := runnerFrom(cmd)
			return r.createInvite(ctx, cmd.Int("max-uses"), cmd.Duration("expires-in"))
		},
	}
}

func inviteListCommand() *cli.Command {
	return &cli.Command{
		Name:  "list",
		Usage: "Show every outstanding invite",
		Description: "Includes exhausted and expired codes. Somebody asking what invites exist is\n" +
			"usually asking why one did not work, and a list that omits the answer is worse than one\n" +
			"they have to read.",
		Flags: configFlags(),
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return runnerFrom(cmd).listInvites(ctx)
		},
	}
}

func inviteRevokeCommand() *cli.Command {
	return &cli.Command{
		Name:      "revoke",
		Usage:     "Delete an invite so it stops working",
		ArgsUsage: "<code>",
		Flags:     configFlags(),
		Action: func(ctx context.Context, cmd *cli.Command) error {
			code := strings.TrimSpace(cmd.Args().First())
			if code == "" {
				return cli.Exit("norite: which invite? Pass the code to revoke.", 2)
			}
			if cmd.Args().Len() > 1 {
				return cli.Exit("norite: revoke takes one code.", 2)
			}
			return runnerFrom(cmd).revokeInvite(ctx, code)
		},
	}
}

// createInvite mints one and prints it.
func (r *Runner) createInvite(ctx context.Context, maxUses int, expiresIn time.Duration) error {
	client, token, err := r.connect()
	if err != nil {
		return err
	}

	body := map[string]any{}
	if maxUses > 0 {
		body["max_uses"] = maxUses
	}
	if expiresIn > 0 {
		// Seconds, because the contract takes seconds — a Go duration string is this client's idea of
		// convenience and has no business crossing the wire.
		body["expires_in_seconds"] = int64(expiresIn.Seconds())
	}

	var invite inviteView
	if err := client.Do(ctx, http.MethodPost, "/api/v1/instance/invites", token, body, &invite); err != nil {
		return err
	}

	if r.JSON {
		return r.printJSON(invite)
	}

	r.printf("%s\n\n", invite.Code)
	r.printf("  uses:    %s\n", describeUses(invite))
	r.printf("  expires: %s\n", describeExpiry(invite))
	return nil
}

// listInvites prints everything outstanding.
func (r *Runner) listInvites(ctx context.Context) error {
	client, token, err := r.connect()
	if err != nil {
		return err
	}

	var invites []inviteView
	if err := client.Do(ctx, http.MethodGet, "/api/v1/instance/invites", token, nil, &invites); err != nil {
		return err
	}

	if r.JSON {
		// An empty list prints `[]`, never `null`. A scripted caller iterating the result should not have
		// to special-case the difference, and Go's nil slice marshals to null without this.
		if invites == nil {
			invites = []inviteView{}
		}
		return r.printJSON(invites)
	}

	if len(invites) == 0 {
		r.printf("No invites. Create one with `norite instance invite create`.\n")
		return nil
	}
	for _, invite := range invites {
		r.printf("%s  %-14s %s\n", invite.Code, describeUses(invite), describeExpiry(invite))
	}
	return nil
}

// revokeInvite deletes one.
func (r *Runner) revokeInvite(ctx context.Context, code string) error {
	client, token, err := r.connect()
	if err != nil {
		return err
	}

	// The code is this client's own argument rather than something the instance said, so it goes into the
	// path as typed — but it is still escaped, because a code with a slash in it would otherwise address a
	// different endpoint entirely.
	path := "/api/v1/instance/invites/" + url.PathEscape(code)
	if err := client.Do(ctx, http.MethodDelete, path, token, nil, nil); err != nil {
		var apiErr *apiclient.Error
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
			return fmt.Errorf("%w\n\nNothing was revoked. Check the code with "+
				"`norite instance invite list`", err)
		}
		return err
	}

	if r.JSON {
		return r.printJSON(map[string]string{"revoked": apiclient.ForDisplay(code)})
	}
	r.printf("Revoked.\n")
	return nil
}

// connect resolves the configuration and mints the operator token every invite call needs.
func (r *Runner) connect() (*apiclient.Client, string, error) {
	cfg, err := r.loadConfig()
	if err != nil {
		return nil, "", err
	}
	baseURL, err := r.resolveInstanceURL(cfg)
	if err != nil {
		return nil, "", err
	}
	token, err := operatorToken(cfg.JWTSecret)
	if err != nil {
		return nil, "", err
	}
	return r.client(baseURL), token, nil
}

// printJSON writes one value as the machine-readable form.
//
// Indented and newline-terminated: this output is read by people as often as by scripts, and jq does not
// care either way.
func (r *Runner) printJSON(v any) error {
	encoded, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding output: %w", err)
	}
	r.printf("%s\n", encoded)
	return nil
}

// describeUses renders the use count the way a person reads it.
func describeUses(in inviteView) string {
	if in.MaxUses == nil {
		return fmt.Sprintf("%d used", in.Uses)
	}
	return fmt.Sprintf("%d of %d used", in.Uses, *in.MaxUses)
}

// describeExpiry says when it stops working, or that it does not.
func describeExpiry(in inviteView) string {
	switch {
	case in.ExpiresAt == nil:
		return "never expires"
	case in.ExpiresAt.Before(time.Now()):
		// Said plainly rather than shown as a past date, because "expired" is the answer somebody is
		// looking for when they ask why a code did not work.
		return "expired"
	default:
		return "expires " + in.ExpiresAt.Local().Format(time.RFC3339)
	}
}
