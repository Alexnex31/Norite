// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package verbs

import (
	"context"
	"net/http"
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
		var n int
		n, err = strconv.Atoi(days)
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
				"unless --max-uses says otherwise. Needs the create-invite permission on the channel.",
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
				var inv apicontract.GuildInvite
				if err := e.do(ctx, http.MethodPost, "/channels/"+cmd.Args().Get(0)+"/invites", body, &inv); err != nil {
					return nil, err
				}
				return inviteFrom(inv), nil
			},
		},
		spec{
			name: "list", usage: "List a guild's live invites, codes in full", ids: []string{"guild"},
			description: "Needs the manage-guild permission, and leaves out invites into channels you cannot see.",
			run: func(ctx context.Context, cmd *cli.Command, e *env) (output.Result, error) {
				var list []apicontract.GuildInvite
				if err := e.do(ctx, http.MethodGet, "/guilds/"+cmd.Args().Get(0)+"/invites", nil, &list); err != nil {
					return nil, err
				}
				out := inviteList{}
				for _, inv := range list {
					out = append(out, inviteFrom(inv))
				}
				return out, nil
			},
		},
		spec{
			name: "show", usage: "See where an invite leads before you use it", ids: []string{codeArg},
			run: func(ctx context.Context, cmd *cli.Command, e *env) (output.Result, error) {
				p, err := with(ctx, e, func(c daemonclient.Caller) (apicontract.GuildInvitePreview, error) {
					return ops.PreviewInvite(ctx, c, cmd.Args().Get(0))
				})
				if err != nil {
					return nil, err
				}
				return previewFrom(p), nil
			},
		},
		spec{
			name: "join", usage: "Join the guild an invite leads to", ids: []string{codeArg},
			description: "Joining a guild you are already in changes nothing and spends no use of the invite.",
			run: func(ctx context.Context, cmd *cli.Command, e *env) (output.Result, error) {
				g, err := with(ctx, e, func(c daemonclient.Caller) (apicontract.Guild, error) {
					return ops.JoinInvite(ctx, c, cmd.Args().Get(0))
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
				code := cmd.Args().Get(0)
				if err := e.do(ctx, http.MethodPost, "/invites/revoke",
					apicontract.GuildInviteCodeRequest{Code: code}, nil); err != nil {
					return nil, err
				}
				return revokedInvite{Revoked: code}, nil
			},
		},
	)
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
}

func inviteFrom(inv apicontract.GuildInvite) inviteView {
	return inviteView{
		ID: inv.Id, Code: inv.Code, GuildID: inv.GuildId, ChannelID: inv.ChannelId, InviterID: inv.InviterId,
		Inviter: userFrom(inv.Inviter), MaxUses: inv.MaxUses, Uses: inv.Uses, ExpiresAt: inv.ExpiresAt,
		CreatedAt: inv.CreatedAt,
	}
}

func (v inviteView) Text(t *output.Text) {
	uses := strconv.Itoa(v.Uses) + " uses"
	if v.MaxUses != nil {
		uses = strconv.Itoa(v.Uses) + " of " + strconv.Itoa(*v.MaxUses) + " uses"
	}
	by := "a deleted account " + c(v.InviterID)
	if v.Inviter != nil {
		by = v.Inviter.named()
	}
	t.Line("%s  into channel %s  %s  %s  by %s", c(v.Code), c(v.ChannelID), uses, expires(v.ExpiresAt), by)
}

func expires(at *time.Time) string {
	if at == nil {
		return "never expires"
	}
	return "expires " + stamp(*at)
}

type inviteList []inviteView

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
}

func previewFrom(p apicontract.GuildInvitePreview) previewView {
	v := previewView{Code: p.Code, ExpiresAt: p.ExpiresAt, Inviter: userFrom(p.Inviter)}
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
	t.Line("Join with `norite invite join %s`.", c(v.Code))
}

// revokedInvite is what `invite revoke` prints: the instance answers with no object, and the code is not an
// id, so the shared `done` does not fit. M10's `instance invite revoke` prints the same shape.
type revokedInvite struct {
	Revoked string `json:"revoked"`
}

func (r revokedInvite) Text(t *output.Text) {
	t.Line("Revoked invite %s.", c(r.Revoked))
}
