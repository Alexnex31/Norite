// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package verbs

import (
	"context"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/urfave/cli/v3"

	"github.com/Alexnex31/Norite/backend/apicontract"
	"github.com/Alexnex31/Norite/cli/internal/clierr"
	"github.com/Alexnex31/Norite/cli/internal/output"
)

// A guild's structure: channels, roles, members and the overwrites between them.

func channelCommand(connect Connector) *cli.Command {
	return group("channel", "List, create, change and delete a guild's channels", connect,
		spec{
			name: "list", usage: "List the guild's channels you can see, in position order",
			ids: []string{"guild"},
			run: func(ctx context.Context, cmd *cli.Command, e *env) (output.Result, error) {
				var channels []apicontract.Channel
				if err := e.do(ctx, http.MethodGet, "/guilds/"+cmd.Args().Get(0)+"/channels", nil,
					&channels); err != nil {
					return nil, err
				}
				out := channelList{}
				for _, ch := range channels {
					out = append(out, channelFrom(ch))
				}
				return out, nil
			},
		},
		spec{
			name: "create", usage: "Create a channel", ids: []string{"guild"},
			description: "Created inside a category with --parent, a channel starts with that category's\n" +
				"permission overwrites copied onto it.",
			flags: []cli.Flag{
				&cli.StringFlag{Name: "name", Usage: "the channel's `NAME` (required)"},
				&cli.StringFlag{Name: "type", Value: "text", Usage: "`text`, voice or category"},
				&cli.StringFlag{Name: "parent", Usage: "the category `ID` to create it in"},
				&cli.StringFlag{Name: "topic", Usage: "what the channel is for"},
				&cli.IntFlag{Name: "position", Usage: "where it sits among its siblings"},
				&cli.BoolFlag{Name: "nsfw", Usage: "mark it age-restricted"},
			},
			run: func(ctx context.Context, cmd *cli.Command, e *env) (output.Result, error) {
				name, err := required(cmd, "name")
				if err != nil {
					return nil, err
				}
				typ, err := channelTypeFlag(cmd.String("type"))
				if err != nil {
					return nil, err
				}
				req := apicontract.CreateChannelRequest{Name: name, Type: typ}
				if req.ParentId, err = flagID(cmd, "parent"); err != nil {
					return nil, err
				}
				if cmd.IsSet("topic") {
					t := cmd.String("topic")
					req.Topic = &t
				}
				if cmd.IsSet("position") {
					p := int(cmd.Int("position"))
					req.Position = &p
				}
				if cmd.IsSet("nsfw") {
					n := cmd.Bool("nsfw")
					req.Nsfw = &n
				}
				var ch apicontract.Channel
				if err := e.do(ctx, http.MethodPost, "/guilds/"+cmd.Args().Get(0)+"/channels", req, &ch); err != nil {
					return nil, err
				}
				return channelFrom(ch), nil
			},
		},
		spec{
			name: "update", usage: "Rename, describe, move or re-mark a channel", ids: []string{"channel"},
			flags: []cli.Flag{
				&cli.StringFlag{Name: "name", Usage: "a new `NAME`"},
				&cli.StringFlag{Name: "topic", Usage: "a new topic"},
				&cli.BoolFlag{Name: "clear-topic", Usage: "remove the topic"},
				&cli.IntFlag{Name: "position", Usage: "a new position among its siblings"},
				&cli.BoolFlag{Name: "nsfw", Usage: "mark it age-restricted, or --nsfw=false to unmark it"},
			},
			run: func(ctx context.Context, cmd *cli.Command, e *env) (output.Result, error) {
				var req apicontract.UpdateChannelRequest
				if cmd.IsSet("topic") && cmd.Bool("clear-topic") {
					return nil, clierr.Usage("--topic and --clear-topic contradict each other; pass one")
				}
				if cmd.IsSet("name") {
					n := cmd.String("name")
					req.Name = &n
				}
				if cmd.IsSet("topic") {
					t := cmd.String("topic")
					req.Topic = &t
				}
				if cmd.Bool("clear-topic") {
					t := true
					req.ClearTopic = &t
				}
				if cmd.IsSet("position") {
					p := int(cmd.Int("position"))
					req.Position = &p
				}
				if cmd.IsSet("nsfw") {
					n := cmd.Bool("nsfw")
					req.Nsfw = &n
				}
				if req == (apicontract.UpdateChannelRequest{}) {
					return nil, clierr.Usage("nothing to change; pass --name, --topic, --clear-topic, " +
						"--position or --nsfw")
				}
				var ch apicontract.Channel
				if err := e.do(ctx, http.MethodPatch, "/channels/"+cmd.Args().Get(0), req, &ch); err != nil {
					return nil, err
				}
				return channelFrom(ch), nil
			},
		},
		spec{
			name: "delete", usage: "Delete a channel and every message in it", ids: []string{"channel"},
			flags: []cli.Flag{yesFlag},
			run: func(ctx context.Context, cmd *cli.Command, e *env) (output.Result, error) {
				id := cmd.Args().Get(0)
				if err := confirm(cmd, e, "delete channel "+id+" and every message in it"); err != nil {
					return nil, err
				}
				if err := e.do(ctx, http.MethodDelete, "/channels/"+id, nil, nil); err != nil {
					return nil, err
				}
				return done{Action: "channel.delete", Target: map[string]string{"channel_id": id}}, nil
			},
		},
	)
}

func channelTypeFlag(s string) (apicontract.CreateChannelRequestType, error) {
	switch strings.ToLower(s) {
	case "text":
		return apicontract.CreateChannelRequestTypeN0, nil
	case "voice":
		return apicontract.CreateChannelRequestTypeN2, nil
	case "category":
		return apicontract.CreateChannelRequestTypeN4, nil
	}
	return 0, clierr.Usage("--type is text, voice or category")
}

// permissions is what a permission bitfield may be on the command line: the decimal string the API takes,
// which `role list` prints.
var permissions = regexp.MustCompile(`^[0-9]{1,19}$`)

func permissionsFlag(cmd *cli.Command, name string) (*string, error) {
	if !cmd.IsSet(name) {
		return nil, nil
	}
	v := cmd.String(name)
	if !permissions.MatchString(v) {
		return nil, clierr.Usage("--%s is a permission bitfield as a decimal number, as `norite role list` prints", name)
	}
	return &v, nil
}

func roleFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{Name: "name", Usage: "the role's `NAME`"},
		&cli.StringFlag{Name: "permissions", Usage: "the permission `BITS`, as a decimal number"},
		&cli.IntFlag{Name: "color", Usage: "the role's color, as an RGB integer"},
		&cli.BoolFlag{Name: "hoist", Usage: "list its members apart in the member list"},
		&cli.BoolFlag{Name: "mentionable", Usage: "let anybody mention it"},
	}
}

func roleCommand(connect Connector) *cli.Command {
	return group("role", "List, create, change, reorder and delete a guild's roles", connect,
		spec{
			name: "list", usage: "List the guild's roles, lowest first", ids: []string{"guild"},
			run: func(ctx context.Context, cmd *cli.Command, e *env) (output.Result, error) {
				var roles []apicontract.Role
				if err := e.do(ctx, http.MethodGet, "/guilds/"+cmd.Args().Get(0)+"/roles", nil, &roles); err != nil {
					return nil, err
				}
				return rolesOf(roles), nil
			},
		},
		spec{
			name: "create", usage: "Create a role, placed just above @everyone", ids: []string{"guild"},
			description: "You can only give a role permissions you hold yourself.",
			flags:       roleFlags(),
			run: func(ctx context.Context, cmd *cli.Command, e *env) (output.Result, error) {
				name, err := required(cmd, "name")
				if err != nil {
					return nil, err
				}
				req := apicontract.CreateRoleRequest{Name: name}
				if req.Permissions, err = permissionsFlag(cmd, "permissions"); err != nil {
					return nil, err
				}
				req.Color, req.Hoist, req.Mentionable = intIfSet(cmd, "color"), boolIfSet(cmd, "hoist"),
					boolIfSet(cmd, "mentionable")
				var r apicontract.Role
				if err := e.do(ctx, http.MethodPost, "/guilds/"+cmd.Args().Get(0)+"/roles", req, &r); err != nil {
					return nil, err
				}
				return roleFrom(r), nil
			},
		},
		spec{
			name: "update", usage: "Change a role", ids: []string{"guild", "role"}, flags: roleFlags(),
			run: func(ctx context.Context, cmd *cli.Command, e *env) (output.Result, error) {
				var req apicontract.UpdateRoleRequest
				var err error
				if cmd.IsSet("name") {
					n := cmd.String("name")
					req.Name = &n
				}
				if req.Permissions, err = permissionsFlag(cmd, "permissions"); err != nil {
					return nil, err
				}
				req.Color, req.Hoist, req.Mentionable = intIfSet(cmd, "color"), boolIfSet(cmd, "hoist"),
					boolIfSet(cmd, "mentionable")
				if req == (apicontract.UpdateRoleRequest{}) {
					return nil, clierr.Usage("nothing to change; pass --name, --permissions, --color, --hoist " +
						"or --mentionable")
				}
				var r apicontract.Role
				if err := e.do(ctx, http.MethodPatch,
					"/guilds/"+cmd.Args().Get(0)+"/roles/"+cmd.Args().Get(1), req, &r); err != nil {
					return nil, err
				}
				return roleFrom(r), nil
			},
		},
		spec{
			name: "reorder", usage: "Move roles to new positions, all at once", ids: []string{"guild"},
			description: "Each --set ROLE=POSITION moves one role; the guild's whole order is renumbered\n" +
				"in one step. You can only move roles below your own highest.",
			flags: []cli.Flag{&cli.StringSliceFlag{Name: "set", Usage: "`ROLE=POSITION`, repeated"}},
			run: func(ctx context.Context, cmd *cli.Command, e *env) (output.Result, error) {
				var req apicontract.ReorderRolesRequest
				for _, pair := range cmd.StringSlice("set") {
					role, pos, ok := strings.Cut(pair, "=")
					n, err := strconv.ParseInt(pos, 10, 32)
					if !ok || !snowflake.MatchString(role) || err != nil || n < 0 {
						return nil, clierr.Usage("--set takes ROLE=POSITION: a role id and a position from 0, "+
							"not %q", output.Clean(pair))
					}
					req.Roles = append(req.Roles, struct {
						Id       apicontract.Snowflake `json:"id"`
						Position int32                 `json:"position"`
					}{Id: role, Position: int32(n)})
				}
				if len(req.Roles) == 0 {
					return nil, clierr.Usage("pass at least one --set ROLE=POSITION")
				}
				var roles []apicontract.Role
				if err := e.do(ctx, http.MethodPatch, "/guilds/"+cmd.Args().Get(0)+"/roles", req, &roles); err != nil {
					return nil, err
				}
				return rolesOf(roles), nil
			},
		},
		spec{
			name: "delete", usage: "Delete a role, taking it from everybody who holds it",
			ids: []string{"guild", "role"}, flags: []cli.Flag{yesFlag},
			run: func(ctx context.Context, cmd *cli.Command, e *env) (output.Result, error) {
				guild, role := cmd.Args().Get(0), cmd.Args().Get(1)
				if err := confirm(cmd, e, "delete role "+role+" from everybody who holds it"); err != nil {
					return nil, err
				}
				if err := e.do(ctx, http.MethodDelete, "/guilds/"+guild+"/roles/"+role, nil, nil); err != nil {
					return nil, err
				}
				return done{Action: "role.delete", Target: map[string]string{"guild_id": guild, "role_id": role}}, nil
			},
		},
	)
}

func rolesOf(roles []apicontract.Role) roleList {
	out := roleList{}
	for _, r := range roles {
		out = append(out, roleFrom(r))
	}
	return out
}

func intIfSet(cmd *cli.Command, name string) *int {
	if !cmd.IsSet(name) {
		return nil
	}
	v := int(cmd.Int(name))
	return &v
}

func boolIfSet(cmd *cli.Command, name string) *bool {
	if !cmd.IsSet(name) {
		return nil
	}
	v := cmd.Bool(name)
	return &v
}

func memberCommand(connect Connector) *cli.Command {
	roleChange := func(name, usage, method string) spec {
		return spec{
			name: name, usage: usage, ids: []string{"guild", "user", "role"},
			run: func(ctx context.Context, cmd *cli.Command, e *env) (output.Result, error) {
				var m apicontract.Member
				path := "/guilds/" + cmd.Args().Get(0) + "/members/" + cmd.Args().Get(1) + "/roles/" + cmd.Args().Get(2)
				if err := e.do(ctx, method, path, nil, &m); err != nil {
					return nil, err
				}
				return memberFrom(m), nil
			},
		}
	}

	members := group("member", "List members, change them, remove them, give and take roles", connect,
		spec{
			name: "list", usage: "List a guild's members, in id order", ids: []string{"guild"},
			flags: []cli.Flag{limitFlag(50), afterFlag("members")},
			run: func(ctx context.Context, cmd *cli.Command, e *env) (output.Result, error) {
				q, limit, err := query(cmd, []string{"after"}, nil)
				if err != nil {
					return nil, err
				}
				var list []apicontract.Member
				if err := e.do(ctx, http.MethodGet, "/guilds/"+cmd.Args().Get(0)+"/members"+q, nil, &list); err != nil {
					return nil, err
				}
				var views []memberView
				for _, m := range list {
					views = append(views, memberFrom(m))
				}
				return memberPage(newPage(views, limit, func(v memberView) string { return v.UserID })), nil
			},
		},
		spec{
			name: "update", usage: "Change a member's nickname, or mute or deafen them",
			ids: []string{"guild", "user"},
			flags: []cli.Flag{
				&cli.StringFlag{Name: "nickname", Usage: "a new `NICKNAME`"},
				&cli.BoolFlag{Name: "clear-nickname", Usage: "remove the nickname"},
				&cli.BoolFlag{Name: "mute", Usage: "server-mute them, or --mute=false to lift it"},
				&cli.BoolFlag{Name: "deaf", Usage: "server-deafen them, or --deaf=false to lift it"},
			},
			run: func(ctx context.Context, cmd *cli.Command, e *env) (output.Result, error) {
				var req apicontract.UpdateMemberRequest
				if cmd.IsSet("nickname") && cmd.Bool("clear-nickname") {
					return nil, clierr.Usage("--nickname and --clear-nickname contradict each other; pass one")
				}
				if cmd.IsSet("nickname") {
					n := cmd.String("nickname")
					req.Nickname = &n
				}
				if cmd.Bool("clear-nickname") {
					t := true
					req.ClearNickname = &t
				}
				req.Mute, req.Deaf = boolIfSet(cmd, "mute"), boolIfSet(cmd, "deaf")
				if req == (apicontract.UpdateMemberRequest{}) {
					return nil, clierr.Usage("nothing to change; pass --nickname, --clear-nickname, --mute or --deaf")
				}
				var m apicontract.Member
				if err := e.do(ctx, http.MethodPatch, "/guilds/"+cmd.Args().Get(0)+"/members/"+cmd.Args().Get(1),
					req, &m); err != nil {
					return nil, err
				}
				return memberFrom(m), nil
			},
		},
		spec{
			name: "remove", usage: "Remove a member from the guild", ids: []string{"guild", "user"},
			flags: []cli.Flag{yesFlag},
			run: func(ctx context.Context, cmd *cli.Command, e *env) (output.Result, error) {
				guild, user := cmd.Args().Get(0), cmd.Args().Get(1)
				if err := confirm(cmd, e, "remove user "+user+" from guild "+guild); err != nil {
					return nil, err
				}
				if err := e.do(ctx, http.MethodDelete, "/guilds/"+guild+"/members/"+user, nil, nil); err != nil {
					return nil, err
				}
				return done{Action: "member.remove", Target: map[string]string{"guild_id": guild, "user_id": user}}, nil
			},
		},
	)
	roleGroup := group("role", "Give a member a role, or take one away", connect,
		roleChange("add", "Give a member a role", http.MethodPut),
		roleChange("remove", "Take a role from a member", http.MethodDelete),
	)
	members.Commands = append(members.Commands, roleGroup)
	return members
}

func overwriteCommand(connect Connector) *cli.Command {
	typeFlag := &cli.StringFlag{Name: "type", Usage: "whether the target is a `role` or a member (required)"}
	targetType := func(cmd *cli.Command) (int, error) {
		switch strings.ToLower(cmd.String("type")) {
		case "role":
			return 0, nil
		case "member":
			return 1, nil
		}
		return 0, clierr.Usage("--type is role or member")
	}

	return group("overwrite", "Set or delete a channel's permission overwrite for a role or a member", connect,
		spec{
			name: "set", usage: "Set what a role or member is allowed and denied in a channel",
			ids: []string{"channel", "target"},
			description: "--allow and --deny are permission bitfields as decimal numbers, and both are\n" +
				"required: an overwrite is replaced whole, so a half left out would be set to 0 and\n" +
				"whatever it carried — a mute's deny, say — lifted without a word. Pass 0 to clear one.",
			flags: []cli.Flag{typeFlag,
				&cli.StringFlag{Name: "allow", Usage: "the permission `BITS` to allow (required; 0 for none)"},
				&cli.StringFlag{Name: "deny", Usage: "the permission `BITS` to deny (required; 0 for none)"},
			},
			run: func(ctx context.Context, cmd *cli.Command, e *env) (output.Result, error) {
				typ, err := targetType(cmd)
				if err != nil {
					return nil, err
				}
				// Both halves, always: PUT replaces the overwrite, and an absent half is stored as 0 — so
				// `--allow 1024` on a muted role's overwrite would lift its deny of sending (M20 /code-review).
				if !cmd.IsSet("allow") || !cmd.IsSet("deny") {
					return nil, clierr.Usage("pass both --allow and --deny: an overwrite is replaced whole, " +
						"so state the half you are not changing too (0 for none)")
				}
				req := apicontract.SetOverwriteRequest{Type: apicontract.SetOverwriteRequestType(typ)}
				if req.Allow, err = permissionsFlag(cmd, "allow"); err != nil {
					return nil, err
				}
				if req.Deny, err = permissionsFlag(cmd, "deny"); err != nil {
					return nil, err
				}
				var o apicontract.PermissionOverwrite
				if err := e.do(ctx, http.MethodPut,
					"/channels/"+cmd.Args().Get(0)+"/permissions/"+cmd.Args().Get(1), req, &o); err != nil {
					return nil, err
				}
				return overwriteFrom(o), nil
			},
		},
		spec{
			name: "delete", usage: "Delete a channel's overwrite for a role or a member",
			ids: []string{"channel", "target"}, flags: []cli.Flag{typeFlag, yesFlag},
			run: func(ctx context.Context, cmd *cli.Command, e *env) (output.Result, error) {
				typ, err := targetType(cmd)
				if err != nil {
					return nil, err
				}
				channel, target := cmd.Args().Get(0), cmd.Args().Get(1)
				// Asked first: deleting an overwrite lifts whatever it denied, which is how a mute is undone.
				if err := confirm(cmd, e, "delete the overwrite for "+target+" in channel "+channel+
					", lifting whatever it denied"); err != nil {
					return nil, err
				}
				req := apicontract.DeleteOverwriteRequest{Type: apicontract.DeleteOverwriteRequestType(typ)}
				if err := e.do(ctx, http.MethodDelete, "/channels/"+channel+"/permissions/"+target, req, nil); err != nil {
					return nil, err
				}
				return done{Action: "overwrite.delete",
					Target: map[string]string{"channel_id": channel, "target_id": target}}, nil
			},
		},
	)
}
