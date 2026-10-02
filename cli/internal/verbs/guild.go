// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package verbs

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/urfave/cli/v3"

	"github.com/Alexnex31/Norite/backend/apicontract"
	"github.com/Alexnex31/Norite/cli/internal/clierr"
	"github.com/Alexnex31/Norite/cli/internal/daemonclient"
	"github.com/Alexnex31/Norite/cli/internal/output"
)

// spec declares one verb; command builds it.
type spec struct {
	name, usage, description string
	ids                      []string
	flags                    []cli.Flag
	run                      func(ctx context.Context, cmd *cli.Command, e *env) (output.Result, error)
}

func (s spec) command(connect Connector) *cli.Command {
	usage, meta := ids(s.ids...)
	return &cli.Command{
		Name: s.name, Usage: s.usage, Description: s.description, ArgsUsage: usage, Metadata: meta,
		Flags: s.flags,
		Action: run(connect, func(ctx context.Context, cmd *cli.Command, e *env) (output.Result, error) {
			r, err := s.run(ctx, cmd, e)
			if err != nil {
				// Which verb failed, ahead of the instance's words: "guild update: not found" rather than a
				// bare "not found". Wrapped, so main still finds the error's kind and its exit code.
				return nil, fmt.Errorf("%s: %w", strings.TrimPrefix(cmd.FullName(), "norite "), err)
			}
			return r, nil
		}),
	}
}

func group(name, usage string, connect Connector, specs ...spec) *cli.Command {
	cmd := &cli.Command{Name: name, Usage: usage}
	for _, s := range specs {
		cmd.Commands = append(cmd.Commands, s.command(connect))
	}
	return cmd
}

// call is daemonclient.Call on the verb's connection.
func (e *env) do(ctx context.Context, method, path string, body, out any) error {
	return daemonclient.Call(ctx, e.call, method, path, body, out)
}

func guildCommand(connect Connector) *cli.Command {
	return group("guild", "List, create, change and delete guilds; read their logs", connect,
		spec{
			name: "list", usage: "List the guilds you are a member of",
			run: func(ctx context.Context, _ *cli.Command, e *env) (output.Result, error) {
				var guilds []apicontract.Guild
				if err := e.do(ctx, http.MethodGet, "/users/@me/guilds", nil, &guilds); err != nil {
					return nil, err
				}
				out := guildList{}
				for _, g := range guilds {
					out = append(out, guildFrom(g))
				}
				return out, nil
			},
		},
		spec{
			name: "create", usage: "Create a guild, which you will own",
			flags: []cli.Flag{
				&cli.StringFlag{Name: "name", Usage: "the guild's `NAME`, 2 to 100 characters (required)"},
				&cli.StringFlag{Name: "description", Usage: "what the guild is for"},
			},
			run: func(ctx context.Context, cmd *cli.Command, e *env) (output.Result, error) {
				name, err := required(cmd, "name")
				if err != nil {
					return nil, err
				}
				req := apicontract.CreateGuildRequest{Name: name}
				if cmd.IsSet("description") {
					d := cmd.String("description")
					req.Description = &d
				}
				var g apicontract.Guild
				if err := e.do(ctx, http.MethodPost, "/guilds", req, &g); err != nil {
					return nil, err
				}
				return guildFrom(g), nil
			},
		},
		spec{
			name: "show", usage: "Show one guild", ids: []string{"guild"},
			run: func(ctx context.Context, cmd *cli.Command, e *env) (output.Result, error) {
				var g apicontract.Guild
				if err := e.do(ctx, http.MethodGet, "/guilds/"+cmd.Args().Get(0), nil, &g); err != nil {
					return nil, err
				}
				return guildFrom(g), nil
			},
		},
		spec{
			name: "update", usage: "Rename a guild, describe it, or switch its message recording",
			description: "Only what is passed changes. --recording on|off is the guild's message recording\n" +
				"(M16b), which only its owner may switch, and switching it is written to the audit log.",
			ids: []string{"guild"},
			flags: []cli.Flag{
				&cli.StringFlag{Name: "name", Usage: "a new `NAME`"},
				&cli.StringFlag{Name: "description", Usage: "a new description"},
				&cli.BoolFlag{Name: "clear-description", Usage: "remove the description"},
				&cli.StringFlag{Name: "recording", Usage: "record every message: `on` or off"},
			},
			run: updateGuild,
		},
		spec{
			name: "delete", usage: "Delete a guild you own, and everything in it", ids: []string{"guild"},
			flags: []cli.Flag{yesFlag},
			run: func(ctx context.Context, cmd *cli.Command, e *env) (output.Result, error) {
				id := cmd.Args().Get(0)
				if err := confirm(cmd, e, "delete guild "+id+" and everything in it"); err != nil {
					return nil, err
				}
				if err := e.do(ctx, http.MethodDelete, "/guilds/"+id, nil, nil); err != nil {
					return nil, err
				}
				return done{Action: "guild.delete", Target: map[string]string{"guild_id": id}}, nil
			},
		},
		spec{
			name: "transfer", usage: "Make another member the guild's owner", ids: []string{"guild", "user"},
			description: "You stop being the owner the moment it succeeds, and only the new owner can give it\n" +
				"back. It needs your own sign-in, never an API token.",
			flags: []cli.Flag{yesFlag},
			run: func(ctx context.Context, cmd *cli.Command, e *env) (output.Result, error) {
				id, user := cmd.Args().Get(0), cmd.Args().Get(1)
				if err := confirm(cmd, e, "hand guild "+id+" to user "+user); err != nil {
					return nil, err
				}
				var g apicontract.Guild
				body := apicontract.TransferGuildOwnershipJSONRequestBody{UserId: user}
				if err := e.do(ctx, http.MethodPost, "/guilds/"+id+"/owner", body, &g); err != nil {
					return nil, err
				}
				return guildFrom(g), nil
			},
		},
		spec{
			name: "audit-log", usage: "Read a guild's audit log, newest first", ids: []string{"guild"},
			flags: []cli.Flag{
				limitFlag(50), beforeFlag("entries"),
				&cli.StringFlag{Name: "action", Usage: "only entries with this `ACTION`, e.g. role.create"},
				&cli.StringFlag{Name: "actor", Usage: "only entries by this user `ID`"},
			},
			run: func(ctx context.Context, cmd *cli.Command, e *env) (output.Result, error) {
				extra := url.Values{}
				if cmd.IsSet("action") {
					extra.Set("action", cmd.String("action"))
				}
				actor, err := flagID(cmd, "actor")
				if err != nil {
					return nil, err
				}
				if actor != nil {
					extra.Set("actor_id", *actor)
				}
				q, limit, err := query(cmd, []string{"before"}, extra)
				if err != nil {
					return nil, err
				}
				var entries []apicontract.AuditLogEntry
				if err := e.do(ctx, http.MethodGet, "/guilds/"+cmd.Args().Get(0)+"/audit-log"+q, nil,
					&entries); err != nil {
					return nil, err
				}
				var views []auditEntryView
				for _, en := range entries {
					views = append(views, auditEntryFrom(en))
				}
				return auditPage(newPage(views, limit, func(v auditEntryView) string { return v.ID })), nil
			},
		},
		spec{
			name: "recording-log", usage: "Read what a recording guild has recorded, newest first",
			ids: []string{"guild"}, flags: []cli.Flag{limitFlag(50), beforeFlag("entries")},
			run: func(ctx context.Context, cmd *cli.Command, e *env) (output.Result, error) {
				q, limit, err := query(cmd, []string{"before"}, nil)
				if err != nil {
					return nil, err
				}
				var entries []apicontract.MessageAuditEntry
				if err := e.do(ctx, http.MethodGet, "/guilds/"+cmd.Args().Get(0)+"/message-audit"+q, nil,
					&entries); err != nil {
					return nil, err
				}
				var views []recordingEntryView
				for _, en := range entries {
					views = append(views, recordingEntryFrom(en))
				}
				return recordingPage(newPage(views, limit, func(v recordingEntryView) string { return v.ID })), nil
			},
		},
	)
}

func updateGuild(ctx context.Context, cmd *cli.Command, e *env) (output.Result, error) {
	var req apicontract.UpdateGuildRequest
	if cmd.IsSet("name") {
		n := cmd.String("name")
		req.Name = &n
	}
	if cmd.IsSet("description") && cmd.Bool("clear-description") {
		return nil, clierr.Usage("--description and --clear-description contradict each other; pass one")
	}
	if cmd.IsSet("description") {
		d := cmd.String("description")
		req.Description = &d
	}
	if cmd.Bool("clear-description") {
		t := true
		req.ClearDescription = &t
	}
	if cmd.IsSet("recording") {
		on, err := onOffFlag(cmd, "recording")
		if err != nil {
			return nil, err
		}
		req.MessageAuditEnabled = &on
	}
	if req == (apicontract.UpdateGuildRequest{}) {
		return nil, clierr.Usage("nothing to change; pass --name, --description, --clear-description or --recording")
	}

	var g apicontract.Guild
	if err := e.do(ctx, http.MethodPatch, "/guilds/"+cmd.Args().Get(0), req, &g); err != nil {
		return nil, err
	}
	return guildFrom(g), nil
}

// onOffFlag reads a flag that takes on or off.
func onOffFlag(cmd *cli.Command, name string) (bool, error) {
	switch strings.ToLower(cmd.String(name)) {
	case "on":
		return true, nil
	case "off":
		return false, nil
	}
	return false, clierr.Usage("--%s takes on or off", name)
}

// required reads a flag the verb cannot do without. Checked here rather than with urfave/cli's Required,
// whose refusal exits 1 with the prefix that makes a missing flag read like a crash.
func required(cmd *cli.Command, name string) (string, error) {
	v := cmd.String(name)
	if !cmd.IsSet(name) || v == "" {
		return "", clierr.Usage("--%s is required", name)
	}
	return v, nil
}
