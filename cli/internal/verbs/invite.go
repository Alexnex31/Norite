// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package verbs

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/urfave/cli/v3"

	"github.com/Alexnex31/Norite/backend/apicontract"
	"github.com/Alexnex31/Norite/cli/internal/clierr"
	"github.com/Alexnex31/Norite/cli/internal/daemonclient"
	"github.com/Alexnex31/Norite/cli/internal/ops"
	"github.com/Alexnex31/Norite/cli/internal/output"
)

// Guild invites (M20a): the way somebody else gets into a guild you are in.
//
// Three of the five take a code rather than an id, as M10's `instance invite revoke` does, and every one
// of them sends it in a request body: a path is written to the instance's request log, and a code is a way
// into the guild (ADR 0029).

// defaultInviteLife is what `invite create` asks for when --expires-in is not given, as the flag spells
// it. The API's own default is no expiry, as instance invites' is; a client choosing a safer one sends it,
// and an invite that lives for ever has to be asked for with `--expires-in never`.
const defaultInviteLife = "7d"

// minInviteLife and maxInviteLife are the instance's bounds on a finite life, checked here so a value it
// would refuse is a usage error before anything is asked.
const (
	minInviteLife = time.Minute
	maxInviteLife = 30 * 24 * time.Hour
)

// parseInviteLife reads --expires-in: "never", or a duration Go understands with days added, since a week is
// written 7d by everybody and 168h by nobody.
func parseInviteLife(raw string) (*time.Duration, error) {
	if raw == "never" {
		return nil, nil
	}
	var d time.Duration
	var err error
	if days, ok := strings.CutSuffix(raw, "d"); ok {
		// Bounded before it is multiplied: a day count past thirty is refused anyway, and an enormous one
		// would overflow the multiplication into a duration that passes (M20a's second /code-review).
		var n int
		if n, err = strconv.Atoi(days); err == nil && (n < 0 || n > 30) {
			err = clierr.Usage("too many days")
		}
		d = time.Duration(n) * 24 * time.Hour
	} else {
		d, err = time.ParseDuration(raw)
	}
	if err != nil || d < minInviteLife || d > maxInviteLife {
		return nil, clierr.Usage("--expires-in takes never, or a duration from 1m to 30d such as 12h or 7d")
	}
	return &d, nil
}

func inviteCommand(connect Connector) *cli.Command {
	return group("invite", "Invite people into a guild, and join one you were invited to", connect,
		spec{
			name: "create", usage: "Create an invite into a channel", ids: []string{"channel"},
			description: "Lives a week unless --expires-in says otherwise, and may be used any number of times " +
				"unless --max-uses says otherwise. Needs the create-invite permission on the channel.\n\n" +
				"Prints the code and a link to send instead of it, which `norite setup` and `norite invite join` " +
				"take whole: the link names this instance, so the person you send it to needs nothing else.",
			flags: []cli.Flag{
				&cli.IntFlag{Name: "max-uses",
					Usage: "how many accounts may join with it, 1 to 1000; unlimited if absent"},
				&cli.StringFlag{Name: "expires-in", Value: defaultInviteLife,
					Usage: "how long it lives: a `DURATION` such as 12h or 7d, up to 30d, or never"},
			},
			run: func(ctx context.Context, cmd *cli.Command, e *env) (output.Result, error) {
				var body apicontract.CreateGuildInviteRequest
				if cmd.IsSet("max-uses") {
					n := int(cmd.Int("max-uses"))
					if n < 1 || n > 1000 {
						return nil, clierr.Usage("--max-uses must be between 1 and 1000")
					}
					body.MaxUses = &n
				}
				life, err := parseInviteLife(cmd.String("expires-in"))
				if err != nil {
					return nil, err
				}
				if life != nil {
					seconds := int64(*life / time.Second)
					body.ExpiresInSeconds = &seconds
				}
				c, err := e.attached(ctx)
				if err != nil {
					return nil, err
				}
				inv, instance, err := ops.CreateInvite(ctx, c, cmd.Args().Get(0), body)
				if err != nil {
					return nil, err
				}
				return inviteFrom(inv, instance), nil
			},
		},
		spec{
			name: "list", usage: "List a guild's live invites, codes in full", ids: []string{"guild"},
			description: "Needs the manage-guild permission, and leaves out invites into channels you cannot see.",
			run: func(ctx context.Context, cmd *cli.Command, e *env) (output.Result, error) {
				c, err := e.attached(ctx)
				if err != nil {
					return nil, err
				}
				list, instance, skipped, err := ops.ListInvites(ctx, c, cmd.Args().Get(0))
				if err != nil {
					return nil, err
				}
				out := inviteListing{rows: inviteList{}, skipped: skipped}
				for _, inv := range list {
					out.rows = append(out.rows, inviteFrom(inv, instance))
				}
				if skipped > 0 && e.json {
					// On stderr, so --json stays the contract's array and a script still sees it said. Text
					// says it beside the rows.
					e.warnf("%s\n", out.leftOut())
				}
				return out, nil
			},
		},
		spec{
			name: "show", usage: "See where an invite leads before you use it", ids: []string{codeArg},
			run: func(ctx context.Context, cmd *cli.Command, e *env) (output.Result, error) {
				inv, err := inviteArg(cmd)
				if err != nil {
					return nil, err
				}
				p, err := with(ctx, e, func(c daemonclient.Caller) (apicontract.GuildInvitePreview, error) {
					return ops.PreviewInvite(ctx, c, inv)
				})
				if err != nil {
					return nil, err
				}
				return previewFrom(p, inv), nil
			},
		},
		spec{
			name: "join", usage: "Join the guild an invite leads to", ids: []string{codeArg},
			description: "Joining a guild you are already in changes nothing and spends no use of the invite.",
			run: func(ctx context.Context, cmd *cli.Command, e *env) (output.Result, error) {
				inv, err := inviteArg(cmd)
				if err != nil {
					return nil, err
				}
				g, err := with(ctx, e, func(c daemonclient.Caller) (apicontract.Guild, error) {
					return ops.JoinInvite(ctx, c, inv)
				})
				if err != nil {
					return nil, err
				}
				return guildFrom(g), nil
			},
		},
		spec{
			name: "revoke", usage: "Delete an invite so it stops working", ids: []string{codeArg},
			description: "Your own invites, or any invite with the manage-guild permission. Not asked about: " +
				"revoking takes a way in away, and an invite is replaced by creating another.",
			run: func(ctx context.Context, cmd *cli.Command, e *env) (output.Result, error) {
				inv, err := inviteArg(cmd)
				if err != nil {
					return nil, err
				}
				if _, err := with(ctx, e, func(c daemonclient.Caller) (struct{}, error) {
					return struct{}{}, ops.RevokeInvite(ctx, c, inv)
				}); err != nil {
					return nil, err
				}
				return revokedInvite{Revoked: inv.Code}, nil
			},
		},
	)
}

// inviteArg is the invite the command was given, a link or a code. checkArgs has already refused what is
// neither. A link's code goes only to the instance it names, which the daemon checks when it sends it: one
// for another instance is a usage error, exit 2, with nothing sent.
func inviteArg(cmd *cli.Command) (ops.Invite, error) {
	inv, err := ops.ParseInvite(cmd.Args().Get(0))
	if err != nil {
		return ops.Invite{}, clierr.Usage("%s", err)
	}
	return inv, nil
}

// ---------- views ----------

type inviteView struct {
	ID        string     `json:"id"`
	Code      string     `json:"code"`
	GuildID   string     `json:"guild_id"`
	ChannelID string     `json:"channel_id"`
	InviterID string     `json:"inviter_id"`
	Inviter   *userView  `json:"inviter"`
	MaxUses   *int       `json:"max_uses"`
	Uses      int        `json:"uses"`
	ExpiresAt *time.Time `json:"expires_at"`
	CreatedAt time.Time  `json:"created_at"`
	// Link is the invite link on the instance that answered, or nil from a daemon that does not say which, or
	// for an instance whose address a link cannot carry (ops.InviteLink).
	Link *string `json:"link"`
}

// inviteFrom is the view of inv, with its link on instance when that is known. inv's code is one the
// instance issues: ops refused or left out any other.
func inviteFrom(inv apicontract.GuildInvite, instance string) inviteView {
	v := inviteView{
		ID: inv.Id, Code: inv.Code, GuildID: inv.GuildId, ChannelID: inv.ChannelId, InviterID: inv.InviterId,
		Inviter: userFrom(inv.Inviter), MaxUses: inv.MaxUses, Uses: inv.Uses, ExpiresAt: inv.ExpiresAt,
		CreatedAt: inv.CreatedAt,
	}
	if link, ok := ops.InviteLink(instance, inv.Code); ok {
		v.Link = &link
	}
	return v
}

// countOf is n and the noun, which is plural but for one (easier-setup's manual pass read "1 uses").
func countOf(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}

func (v inviteView) Text(t *output.Text) {
	uses := countOf(v.Uses, "use")
	if v.MaxUses != nil {
		uses = strconv.Itoa(v.Uses) + " of " + countOf(*v.MaxUses, "use")
	}
	by := "a deleted account " + c(v.InviterID)
	if v.Inviter != nil {
		by = v.Inviter.named()
	}
	t.Line("%s  into channel %s  %s  %s  by %s", c(v.Code), c(v.ChannelID), uses, expires(v.ExpiresAt), by)
	if v.Link == nil {
		return
	}
	t.Line("  link  %s", c(*v.Link))
	if ops.LocalOnly(*v.Link) {
		// The link is built from the address this machine signs in with, and that one names this machine:
		// sent to anybody else, it points at their own.
		t.Line("  That address reaches this machine alone, so the link works only here. Send the code, with the")
		t.Line("  address the others use to reach this instance.")
	}
}

func expires(at *time.Time) string {
	if at == nil {
		return "never expires"
	}
	return "expires " + stamp(*at)
}

type inviteList []inviteView

// inviteListing is `invite list`'s result: the rows, which are its JSON, and how many the instance listed
// that were left out (ops.ListInvites), which text says beside them.
type inviteListing struct {
	rows    inviteList
	skipped int
}

func (l inviteListing) MarshalJSON() ([]byte, error) { return json.Marshal(l.rows) }

func (l inviteListing) Text(t *output.Text) {
	switch {
	case len(l.rows) == 0 && l.skipped == 0:
		t.Line("No live invites.")
	case len(l.rows) == 0:
		t.Line("No live invite to show.")
	}
	for _, v := range l.rows {
		v.Text(t)
	}
	if l.skipped > 0 {
		t.Line("%s", l.leftOut())
	}
}

// leftOut says how many invites were left out, and why.
func (l inviteListing) leftOut() string {
	if l.skipped == 1 {
		return "1 invite left out: its code is not one the instance issues."
	}
	return strconv.Itoa(l.skipped) + " invites left out: their codes are not ones the instance issues."
}

func (l inviteList) Text(t *output.Text) {
	if len(l) == 0 {
		t.Line("No live invites.")
	}
	for _, v := range l {
		v.Text(t)
	}
}

type previewView struct {
	Code      string     `json:"code"`
	ExpiresAt *time.Time `json:"expires_at"`
	Guild     struct {
		ID          string  `json:"id"`
		Name        string  `json:"name"`
		Description *string `json:"description"`
		IconHash    *string `json:"icon_hash"`
	} `json:"guild"`
	Channel struct {
		ID   string  `json:"id"`
		Name *string `json:"name"`
	} `json:"channel"`
	Inviter *userView `json:"inviter"`
	// join is what to give `norite invite join` for this invite: its link when one was given, rebuilt from
	// the link's instance and the code the instance issues, or that code. Empty when no link can be built.
	join string
}

// previewFrom is the preview of inv, the invite the command was given. p's code is one the instance issues
// (ops.PreviewInvite).
func previewFrom(p apicontract.GuildInvitePreview, inv ops.Invite) previewView {
	v := previewView{Code: p.Code, ExpiresAt: p.ExpiresAt, Inviter: userFrom(p.Inviter), join: p.Code}
	if inv.Instance != "" {
		// A link keeps its instance, which a bare code would drop: given one, the command joins a link.
		link, _ := ops.InviteLink(inv.Instance, p.Code)
		v.join = link
	}
	v.Guild.ID, v.Guild.Name, v.Guild.Description, v.Guild.IconHash =
		p.Guild.Id, p.Guild.Name, p.Guild.Description, p.Guild.IconHash
	v.Channel.ID, v.Channel.Name = p.Channel.Id, p.Channel.Name
	return v
}

// Text is the preview as a person deciding whether to join reads it. Every value but the ids is a
// stranger's text — the guild's name and description, the channel's name, the inviter's names — and each
// passes termsafe (rule 19).
func (v previewView) Text(t *output.Text) {
	t.Line("%s  %s", c(v.Guild.Name), c(v.Guild.ID))
	if v.Guild.Description != nil && *v.Guild.Description != "" {
		indented(t, *v.Guild.Description)
	}
	t.Line("  lands in #%s  %s", output.OrNone(v.Channel.Name), c(v.Channel.ID))
	if v.Inviter != nil {
		t.Line("  invited by %s", v.Inviter.named())
	} else {
		t.Line("  invited by a deleted account")
	}
	t.Line("  %s", expires(v.ExpiresAt))
	// Rebuilt from what was checked, rather than repeated as typed: what a person types may hold a tab,
	// which termsafe would turn into something no command takes. Quoted all the same.
	if v.join != "" {
		t.Line("Join with `norite invite join %s`.", output.ShellWord(c(v.join)))
	}
}

// revokedInvite is what `invite revoke` prints: the instance answers with no object, and the code is not an
// id, so the shared `done` does not fit. M10's `instance invite revoke` prints the same shape.
type revokedInvite struct {
	Revoked string `json:"revoked"`
}

func (r revokedInvite) Text(t *output.Text) {
	t.Line("Revoked invite %s.", c(r.Revoked))
}
