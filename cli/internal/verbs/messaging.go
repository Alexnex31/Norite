// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package verbs

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/urfave/cli/v3"

	"github.com/Alexnex31/Norite/backend/apicontract"
	"github.com/Alexnex31/Norite/cli/internal/clierr"
	"github.com/Alexnex31/Norite/cli/internal/daemonclient"
	"github.com/Alexnex31/Norite/cli/internal/ops"
	"github.com/Alexnex31/Norite/cli/internal/output"
)

// What is said in a guild, and what is done about it: messages, reports and tags.

// content reads a message's text from --content, or from stdin when --content is "-": a message is the
// one argument long enough, and multi-line enough, that a shell's quoting gets in the way.
func content(cmd *cli.Command, e *env) (string, error) {
	text, err := required(cmd, "content")
	if err != nil {
		return "", err
	}
	if text == "-" {
		raw, err := io.ReadAll(io.LimitReader(e.in, 4*ops.MaxContent+1))
		if err != nil {
			return "", err
		}
		text = strings.TrimRight(string(raw), "\n")
	}
	if err := ops.CheckContent(text); err != nil {
		return "", err
	}
	return text, nil
}

var contentFlag = &cli.StringFlag{Name: "content", Usage: "the message's `TEXT`, or - to read it from stdin (required)"}

func messageCommand(connect Connector) *cli.Command {
	return group("message", "Read, post, edit and delete messages; read a message's earlier versions", connect,
		spec{
			name: "list", usage: "Read a channel's messages, newest first", ids: []string{"channel"},
			description: "Pages backwards: pass the next page's --before. --after keeps only messages newer\n" +
				"than an id, still newest first.",
			flags: []cli.Flag{limitFlag(50), beforeFlag("messages"), afterFlag("messages")},
			run: func(ctx context.Context, cmd *cli.Command, e *env) (output.Result, error) {
				page, err := pageOf(cmd)
				if err != nil {
					return nil, err
				}
				list, err := with(ctx, e, func(c daemonclient.Caller) ([]apicontract.Message, error) {
					return ops.ListMessages(ctx, c, cmd.Args().Get(0), page)
				})
				if err != nil {
					return nil, err
				}
				limit := page.Limit
				var views []messageView
				for _, m := range list {
					views = append(views, messageFrom(m))
				}
				return messagePage(newPage(views, limit, func(v messageView) string { return v.ID })), nil
			},
		},
		spec{
			name: "send", usage: "Post a message", ids: []string{"channel"},
			flags: []cli.Flag{contentFlag, &cli.StringFlag{Name: "reply-to", Usage: "the message `ID` this answers"}},
			run: func(ctx context.Context, cmd *cli.Command, e *env) (output.Result, error) {
				text, err := content(cmd, e)
				if err != nil {
					return nil, err
				}
				replyTo, err := flagID(cmd, "reply-to")
				if err != nil {
					return nil, err
				}
				m, err := with(ctx, e, func(c daemonclient.Caller) (apicontract.Message, error) {
					return ops.SendMessage(ctx, c, cmd.Args().Get(0), text, replyTo)
				})
				if err != nil {
					return nil, err
				}
				return messageFrom(m), nil
			},
		},
		spec{
			name: "edit", usage: "Change what one of your messages says", ids: []string{"channel", "message"},
			description: "Only a message's author may edit it. What it said before is kept, and a moderator\n" +
				"can read it with `norite message history`.",
			flags: []cli.Flag{contentFlag},
			run: func(ctx context.Context, cmd *cli.Command, e *env) (output.Result, error) {
				text, err := content(cmd, e)
				if err != nil {
					return nil, err
				}
				var m apicontract.Message
				if err := e.do(ctx, http.MethodPatch, "/channels/"+cmd.Args().Get(0)+"/messages/"+cmd.Args().Get(1),
					apicontract.UpdateMessageJSONRequestBody{Content: text}, &m); err != nil {
					return nil, err
				}
				return messageFrom(m), nil
			},
		},
		spec{
			name: "delete", usage: "Delete a message: your own, or as a moderator anybody's",
			ids: []string{"channel", "message"}, flags: []cli.Flag{yesFlag},
			run: func(ctx context.Context, cmd *cli.Command, e *env) (output.Result, error) {
				channel, message := cmd.Args().Get(0), cmd.Args().Get(1)
				if err := confirm(cmd, e, "delete message "+message); err != nil {
					return nil, err
				}
				if err := e.do(ctx, http.MethodDelete, "/channels/"+channel+"/messages/"+message, nil, nil); err != nil {
					return nil, err
				}
				return done{Action: "message.delete",
					Target: map[string]string{"channel_id": channel, "message_id": message}}, nil
			},
		},
		spec{
			name: "history", usage: "Read a message's earlier versions, newest first, as a moderator",
			ids: []string{"channel", "message"}, flags: []cli.Flag{limitFlag(50), beforeFlag("versions")},
			run: func(ctx context.Context, cmd *cli.Command, e *env) (output.Result, error) {
				q, limit, err := query(cmd, []string{"before"}, nil)
				if err != nil {
					return nil, err
				}
				var h apicontract.MessageEditHistory
				if err := e.do(ctx, http.MethodGet,
					"/channels/"+cmd.Args().Get(0)+"/messages/"+cmd.Args().Get(1)+"/history"+q, nil, &h); err != nil {
					return nil, err
				}
				v := historyView{
					MessageID: h.MessageId, ChannelID: h.ChannelId, AuthorID: h.AuthorId,
					CurrentContent: h.CurrentContent, EditedAt: h.EditedAt, DeletedAt: h.DeletedAt, E2E: h.IsE2e,
					Versions: []versionView{},
				}
				for _, ver := range h.Versions {
					v.Versions = append(v.Versions, versionView{ID: ver.Id, Content: ver.Content, EditedAt: ver.EditedAt})
				}
				v.Next = newPage(v.Versions, limit, func(x versionView) string { return x.ID }).Next
				return v, nil
			},
		},
	)
}

var reasons = []string{"spam", "harassment", "hate_speech", "violence", "nsfw", "self_harm", "illegal", "other"}

func reportCommand(connect Connector) *cli.Command {
	resolve := func(name, usage, status string) spec {
		return spec{
			name: name, usage: usage, ids: []string{"guild", "report"},
			run: func(ctx context.Context, cmd *cli.Command, e *env) (output.Result, error) {
				var r apicontract.Report
				body := apicontract.ResolveReportRequest{Status: apicontract.ResolveReportRequestStatus(status)}
				if err := e.do(ctx, http.MethodPost,
					"/guilds/"+cmd.Args().Get(0)+"/reports/"+cmd.Args().Get(1)+"/resolve", body, &r); err != nil {
					return nil, err
				}
				return reportFrom(r), nil
			},
		}
	}

	return group("report", "File a report, and triage a guild's reports as a moderator", connect,
		spec{
			name: "file", usage: "Report a message to its guild's moderators", ids: []string{"message"},
			description: "The moderators reading it are never told who filed it.\n\n" +
				"--reason is one of " + strings.Join(reasons, ", ") + ".",
			flags: []cli.Flag{
				&cli.StringFlag{Name: "reason", Usage: "why: `REASON`, one of the list above (required)"},
				&cli.StringFlag{Name: "detail", Usage: "anything the moderators should know"},
			},
			run: func(ctx context.Context, cmd *cli.Command, e *env) (output.Result, error) {
				reason, err := required(cmd, "reason")
				if err != nil {
					return nil, err
				}
				if !slices.Contains(reasons, reason) {
					return nil, clierr.Usage("--reason is one of %s", strings.Join(reasons, ", "))
				}
				body := apicontract.FileReportRequest{
					TargetType: apicontract.ReportTargetTypeMessage, TargetId: cmd.Args().Get(0),
					ReasonCategory: apicontract.ReportReasonCategory(reason),
				}
				if cmd.IsSet("detail") {
					d := cmd.String("detail")
					body.Detail = &d
				}
				var r apicontract.Report
				if err := e.do(ctx, http.MethodPost, "/reports", body, &r); err != nil {
					return nil, err
				}
				return reportFrom(r), nil
			},
		},
		spec{
			name: "list", usage: "List a guild's reports, newest first", ids: []string{"guild"},
			flags: []cli.Flag{limitFlag(50), beforeFlag("reports"),
				&cli.StringFlag{Name: "status", Usage: "only reports in this `STATUS`: open, resolved or dismissed"}},
			run: func(ctx context.Context, cmd *cli.Command, e *env) (output.Result, error) {
				extra := url.Values{}
				if cmd.IsSet("status") {
					extra.Set("status", cmd.String("status"))
				}
				q, limit, err := query(cmd, []string{"before"}, extra)
				if err != nil {
					return nil, err
				}
				var list []apicontract.TriageReport
				if err := e.do(ctx, http.MethodGet, "/guilds/"+cmd.Args().Get(0)+"/reports"+q, nil, &list); err != nil {
					return nil, err
				}
				var views []triageView
				for _, r := range list {
					views = append(views, triageFrom(r))
				}
				return triagePage(newPage(views, limit, func(v triageView) string { return v.ID })), nil
			},
		},
		spec{
			name: "show", usage: "Open one report, with what its target says now", ids: []string{"guild", "report"},
			run: func(ctx context.Context, cmd *cli.Command, e *env) (output.Result, error) {
				var r apicontract.TriageReportDetail
				if err := e.do(ctx, http.MethodGet, "/guilds/"+cmd.Args().Get(0)+"/reports/"+cmd.Args().Get(1),
					nil, &r); err != nil {
					return nil, err
				}
				return triageDetailFrom(r), nil
			},
		},
		resolve("resolve", "Close a report as acted on", "resolved"),
		resolve("dismiss", "Close a report as needing nothing", "dismissed"),
	)
}

func tagCommand(connect Connector) *cli.Command {
	tagOnMessage := func(name, usage, method, action string) spec {
		return spec{
			name: name, usage: usage, ids: []string{"channel", "message", "tag"},
			run: func(ctx context.Context, cmd *cli.Command, e *env) (output.Result, error) {
				channel, message, tag := cmd.Args().Get(0), cmd.Args().Get(1), cmd.Args().Get(2)
				if err := e.do(ctx, method, "/channels/"+channel+"/messages/"+message+"/tags/"+tag, nil, nil); err != nil {
					return nil, err
				}
				return done{Action: action,
					Target: map[string]string{"channel_id": channel, "message_id": message, "tag_id": tag}}, nil
			},
		}
	}

	return group("tag", "Create a guild's tags and put them on messages", connect,
		spec{
			name: "list", usage: "List the guild's shared tags and your private ones", ids: []string{"guild"},
			run: func(ctx context.Context, cmd *cli.Command, e *env) (output.Result, error) {
				var list []apicontract.MessageTag
				if err := e.do(ctx, http.MethodGet, "/guilds/"+cmd.Args().Get(0)+"/tags", nil, &list); err != nil {
					return nil, err
				}
				out := tagList{}
				for _, m := range list {
					out = append(out, tagFrom(m))
				}
				return out, nil
			},
		},
		spec{
			name: "create", usage: "Create a tag: private to you, or shared with the guild", ids: []string{"guild"},
			flags: []cli.Flag{
				&cli.StringFlag{Name: "name", Usage: "the tag's `NAME` (required)"},
				&cli.BoolFlag{Name: "shared", Usage: "share it with the guild; needs the moderation permission"},
			},
			run: func(ctx context.Context, cmd *cli.Command, e *env) (output.Result, error) {
				name, err := required(cmd, "name")
				if err != nil {
					return nil, err
				}
				body := apicontract.CreateMessageTagRequest{Name: name, IsShared: boolIfSet(cmd, "shared")}
				var m apicontract.MessageTag
				if err := e.do(ctx, http.MethodPost, "/guilds/"+cmd.Args().Get(0)+"/tags", body, &m); err != nil {
					return nil, err
				}
				return tagFrom(m), nil
			},
		},
		spec{
			name: "delete", usage: "Delete a tag, taking it off every message it is on",
			ids: []string{"guild", "tag"}, flags: []cli.Flag{yesFlag},
			run: func(ctx context.Context, cmd *cli.Command, e *env) (output.Result, error) {
				guild, tag := cmd.Args().Get(0), cmd.Args().Get(1)
				if err := confirm(cmd, e, "delete tag "+tag+" from every message it is on"); err != nil {
					return nil, err
				}
				if err := e.do(ctx, http.MethodDelete, "/guilds/"+guild+"/tags/"+tag, nil, nil); err != nil {
					return nil, err
				}
				return done{Action: "tag.delete", Target: map[string]string{"guild_id": guild, "tag_id": tag}}, nil
			},
		},
		tagOnMessage("apply", "Put a tag on a message", http.MethodPut, "tag.apply"),
		tagOnMessage("unapply", "Take a tag off a message", http.MethodDelete, "tag.remove"),
		spec{
			name: "on", usage: "List the tags on a message that you can see", ids: []string{"channel", "message"},
			run: func(ctx context.Context, cmd *cli.Command, e *env) (output.Result, error) {
				var list []apicontract.AppliedMessageTag
				if err := e.do(ctx, http.MethodGet,
					"/channels/"+cmd.Args().Get(0)+"/messages/"+cmd.Args().Get(1)+"/tags", nil, &list); err != nil {
					return nil, err
				}
				out := appliedTagList{}
				for _, a := range list {
					out = append(out, appliedTagFrom(a))
				}
				return out, nil
			},
		},
	)
}
