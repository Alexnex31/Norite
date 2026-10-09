// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package verbs

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/urfave/cli/v3"

	"github.com/Alexnex31/Norite/backend/apicontract"
	"github.com/Alexnex31/Norite/cli/internal/clierr"
	"github.com/Alexnex31/Norite/cli/internal/output"
)

// API tokens (M22): the credential a script holds, bounded by the scopes it was minted with.
//
// These are the only verbs that reach /auth, through the one exception the relay makes (relay's package
// comment). The instance asks for the account's own live sign-in on all three, which the daemon has; an
// API token can neither mint, list nor revoke.
//
// A token's value exists once, in the answer to `create`. It is the one secret this command tree prints,
// because there is no other moment to hand it over, and --out is there for whoever would rather it never
// crossed a terminal. It is never taken from a flag and never written to stderr or a log.

// maxTokenName is the instance's bound on a token's name.
const maxTokenName = 100

// tokenFileMode is the mode --out creates its file with: the value is a credential from its first byte.
const tokenFileMode fs.FileMode = 0o600

func tokenCommand(connect Connector) *cli.Command {
	return group("token", "Create, list and revoke API tokens for scripts and bots", connect,
		spec{
			name: "create", usage: "Create a token limited to the scopes you name",
			description: "Prints the token's value once; the instance keeps only a hash, so a lost value is " +
				"replaced, never recovered. With --out the value goes to a new file only you can read, and " +
				"is not printed. A scope only narrows what the token may do: it never reaches past your own " +
				"permissions. Messages a token sends or edits are marked as automated for everyone who " +
				"reads them.",
			flags: []cli.Flag{
				&cli.StringFlag{Name: "name", Usage: "what to call it, so you can tell it apart when revoking"},
				&cli.StringSliceFlag{Name: "scope", Usage: "a `SCOPE` to grant; repeat for several: " + scopeList()},
				&cli.StringFlag{Name: "out", Usage: "write the value to this new `FILE` instead of printing it"},
			},
			run: createToken,
		},
		spec{
			name: "list", usage: "List your tokens, without their values",
			run: func(ctx context.Context, _ *cli.Command, e *env) (output.Result, error) {
				var list []apicontract.ApiToken
				if err := e.do(ctx, http.MethodGet, "/auth/tokens", nil, &list); err != nil {
					return nil, err
				}
				out := tokenList{}
				for _, tok := range list {
					out = append(out, tokenFrom(tok.Id, tok.Name, tok.Scopes, tok.LastUsedAt, tok.CreatedAt))
				}
				return out, nil
			},
		},
		spec{
			name: "revoke", usage: "Revoke a token, so whatever holds it stops working", ids: []string{"token"},
			flags: []cli.Flag{yesFlag},
			run: func(ctx context.Context, cmd *cli.Command, e *env) (output.Result, error) {
				id := cmd.Args().Get(0)
				if err := confirm(cmd, e, "revoke token "+id+", stopping whatever uses it"); err != nil {
					return nil, err
				}
				if err := e.do(ctx, http.MethodDelete, "/auth/tokens/"+id, nil, nil); err != nil {
					return nil, err
				}
				return done{Action: "token.revoke", Target: map[string]string{"token_id": id}}, nil
			},
		},
	)
}

// allScopes is every scope the contract declares, in the order the help lists them. The contract's own
// enum decides what is valid (apicontract.Scope.Valid); TestTheScopeHelpListsEveryScope holds this to it.
var allScopes = []apicontract.Scope{
	apicontract.Identify,
	apicontract.GuildsRead, apicontract.GuildsWrite, apicontract.GuildsAudit,
	apicontract.MessagesRead, apicontract.MessagesWrite, apicontract.MessagesModerate, apicontract.MessagesAudit,
	apicontract.ReportsWrite, apicontract.ReportsModerate,
	apicontract.TagsRead, apicontract.TagsWrite,
}

func scopeList() string {
	names := make([]string, len(allScopes))
	for i, s := range allScopes {
		names[i] = string(s)
	}
	return strings.Join(names, ", ")
}

func createToken(ctx context.Context, cmd *cli.Command, e *env) (output.Result, error) {
	name := strings.TrimSpace(cmd.String("name"))
	if name == "" || utf8.RuneCountInString(name) > maxTokenName {
		return nil, clierr.Usage("--name is required, and at most %d characters", maxTokenName)
	}
	asked := cmd.StringSlice("scope")
	if len(asked) == 0 {
		return nil, clierr.Usage("name at least one --scope: %s", scopeList())
	}
	body := apicontract.MintApiTokenRequest{Name: name}
	for _, raw := range asked {
		s := apicontract.Scope(raw)
		if !s.Valid() {
			return nil, clierr.Usage("%q is not a scope; the scopes are %s", output.Clean(raw), scopeList())
		}
		body.Scopes = append(body.Scopes, s)
	}

	// The file is claimed before the token exists. A value minted and then not written would be a live
	// credential nobody holds: it cannot be shown again, only revoked by somebody who notices it in a list.
	var file *os.File
	if path := cmd.String("out"); path != "" {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, tokenFileMode)
		if err != nil {
			if errors.Is(err, fs.ErrExist) {
				return nil, clierr.Usage("%s already exists; --out writes a new file and replaces nothing",
					output.Clean(path))
			}
			return nil, clierr.Usage("cannot create %s: %s", output.Clean(path), output.Clean(reason(err)))
		}
		file = f
	}
	discard := func() {
		if file != nil {
			_ = file.Close()
			_ = os.Remove(file.Name())
		}
	}

	var minted apicontract.MintedApiToken
	if err := e.do(ctx, http.MethodPost, "/auth/tokens", body, &minted); err != nil {
		discard()
		return nil, err
	}
	// A valid body with no value is an answer this client cannot use, and saying "created" over it would
	// leave somebody looking for a credential that was never handed over.
	if minted.Value == "" {
		discard()
		return nil, fmt.Errorf("the instance answered without the token's value; revoke token %s and "+
			"create another", output.Clean(minted.Id))
	}

	v := mintedView{tokenView: tokenFrom(minted.Id, minted.Name, minted.Scopes, minted.LastUsedAt, minted.CreatedAt)}
	if file == nil {
		v.Value = &minted.Value
		return v, nil
	}
	_, werr := file.WriteString(minted.Value + "\n")
	if cerr := file.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		// The token exists and its value is in hand, so it is given the only way left rather than lost.
		_ = os.Remove(file.Name())
		v.Value = &minted.Value
		v.note = "could not write " + file.Name() + " (" + reason(werr) + "), so the value is printed instead"
		return v, nil
	}
	written := file.Name()
	v.WrittenTo = &written
	return v, nil
}

// reason is an OS error without the path it repeats, which the message around it already names.
func reason(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return err.Error()
}

// ---------- views ----------

type tokenView struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Scopes     []string   `json:"scopes"`
	LastUsedAt *time.Time `json:"last_used_at"`
	CreatedAt  time.Time  `json:"created_at"`
}

func tokenFrom(id, name string, scopes []apicontract.Scope, used *time.Time, created time.Time) tokenView {
	v := tokenView{ID: id, Name: name, Scopes: []string{}, LastUsedAt: used, CreatedAt: created}
	for _, s := range scopes {
		v.Scopes = append(v.Scopes, string(s))
	}
	return v
}

// line is a token as a list shows it. The name is the user's own text, read back from an instance, and a
// scope is the instance's: both pass termsafe (rule 19).
func (v tokenView) line() string {
	scopes := make([]string, len(v.Scopes))
	for i, s := range v.Scopes {
		scopes[i] = c(s)
	}
	used := "never used"
	if v.LastUsedAt != nil {
		used = "last used " + stamp(*v.LastUsedAt)
	}
	return fmt.Sprintf("%s  %s  [%s]  created %s  %s",
		c(v.ID), c(v.Name), strings.Join(scopes, ", "), stamp(v.CreatedAt), used)
}

func (v tokenView) Text(t *output.Text) { t.Line("%s", v.line()) }

type tokenList []tokenView

func (l tokenList) Text(t *output.Text) {
	if len(l) == 0 {
		t.Line("No tokens.")
	}
	for _, v := range l {
		v.Text(t)
	}
}

// mintedView is `token create`'s result: the token, and its value or where the value went, never both.
type mintedView struct {
	tokenView
	// Value is the credential, or null when --out took it.
	Value *string `json:"value"`
	// WrittenTo is the file --out wrote, or null when the value is in Value.
	WrittenTo *string `json:"written_to"`

	// note says why a value --out asked to keep off the screen is on it. Text only: the JSON's value and
	// written_to already say what happened.
	note string
}

func (v mintedView) Text(t *output.Text) {
	t.Line("%s", v.line())
	if v.WrittenTo != nil {
		t.Line("Its value is in %s, and nowhere else: it cannot be shown again.", c(*v.WrittenTo))
		return
	}
	if v.note != "" {
		t.Line("%s.", c(v.note))
	}
	t.Line("%s", c(*v.Value))
	t.Line("That is the token, shown this once. Keep it as you would a password.")
}
