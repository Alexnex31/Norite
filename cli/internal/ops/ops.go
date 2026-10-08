// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package ops is what the account does through the daemon, once, for every front end that does it (M20a).
//
// The command tree's verbs and the terminal client both list guilds, channels and messages, send a message,
// and preview and redeem an invite. Each of those is a function here taking a daemonclient.Caller and plain
// arguments, so the request's shape, the instance's bounds and the mapping of every outcome to an error exist
// once rather than once per front end (ADR 0026's one command tree, carried below the command line). The
// verbs add their flags and views on top; the client adds its drawing.
//
// What a function returns is the instance's own type, from backend/apicontract. Text in it is a stranger's
// and reaches a terminal only through termsafe (rule 19); that is the caller's job, because only the caller
// knows how it is drawn.
package ops

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Alexnex31/Norite/backend/apicontract"
	"github.com/Alexnex31/Norite/cli/internal/clierr"
	"github.com/Alexnex31/Norite/cli/internal/daemonclient"
)

// MaxContent is the instance's own bound on a message, in runes, which is what its validator counts: 4,000
// Japanese characters fit, though they are 12,000 bytes (M15).
const MaxContent = 4000

// MaxPage is the instance's bound on one page of messages.
const MaxPage = 100

// MessageTypeAutomation is messages.type's value for a message an API token sent or has ever edited (M22),
// and a webhook's from M60. The instance sets it. Both front ends mark such a message, and read the value
// here so that they cannot come to disagree about which one it is.
const MessageTypeAutomation = 1

var snowflake = regexp.MustCompile(`^[0-9]{1,20}$`)

// IsID reports whether s can be a snowflake: digits, and no more than a 64-bit integer has.
//
// Every id this package puts into a path is checked with it, whoever supplied the id. A verb's come from the
// command line and were checked already; the client's come from the instance, a stranger's server, and an
// id like "../auth/logout" would otherwise become a request path. The relay refuses such a path too; this is
// the first line, as it is for the verbs.
func IsID(s string) bool { return snowflake.MatchString(s) }

func checkIDs(ids ...string) error {
	for _, id := range ids {
		if !IsID(id) {
			return fmt.Errorf("%q is not an id the instance could have given", id)
		}
	}
	return nil
}

// CheckContent refuses what the instance would: an empty message, or one longer than MaxContent runes.
// Refused here so a client can say so without a round trip, and a verb can say so as a usage error.
func CheckContent(text string) error {
	if strings.TrimSpace(text) == "" {
		return clierr.Usage("the message is empty")
	}
	if n := utf8.RuneCountInString(text); n > MaxContent {
		return clierr.Usage("the message is %d characters; the instance takes at most %d", n, MaxContent)
	}
	return nil
}

// ListGuilds returns every guild the account is in.
func ListGuilds(ctx context.Context, c daemonclient.Caller) ([]apicontract.Guild, error) {
	var out []apicontract.Guild
	if err := daemonclient.Call(ctx, c, http.MethodGet, "/users/@me/guilds", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ListChannels returns the channels of a guild the account can see, every type of them.
func ListChannels(ctx context.Context, c daemonclient.Caller, guildID string) ([]apicontract.Channel, error) {
	if err := checkIDs(guildID); err != nil {
		return nil, err
	}
	var out []apicontract.Channel
	if err := daemonclient.Call(ctx, c, http.MethodGet, "/guilds/"+guildID+"/channels", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Page bounds one read of a channel's backlog. Limit is 1 to MaxPage; Before and After are message ids,
// either or both, and the instance applies them together as a window, newest first.
type Page struct {
	Limit         int
	Before, After *string
}

// ListMessages returns one page of a channel's messages, newest first.
func ListMessages(ctx context.Context, c daemonclient.Caller, channelID string, p Page) ([]apicontract.Message, error) {
	if err := checkIDs(channelID); err != nil {
		return nil, err
	}
	if p.Limit < 1 || p.Limit > MaxPage {
		return nil, clierr.Usage("a page holds 1 to %d messages", MaxPage)
	}
	q := url.Values{"limit": {strconv.Itoa(p.Limit)}}
	for name, id := range map[string]*string{"before": p.Before, "after": p.After} {
		if id == nil {
			continue
		}
		if err := checkIDs(*id); err != nil {
			return nil, err
		}
		q.Set(name, *id)
	}
	var out []apicontract.Message
	if err := daemonclient.Call(ctx, c, http.MethodGet, "/channels/"+channelID+"/messages?"+q.Encode(), nil,
		&out); err != nil {
		return nil, err
	}
	return out, nil
}

// SendMessage posts text to a channel, as a reply to replyTo when it is not nil, and returns the message as
// the instance stored it.
func SendMessage(
	ctx context.Context, c daemonclient.Caller, channelID, text string, replyTo *string,
) (apicontract.Message, error) {
	if err := checkIDs(channelID); err != nil {
		return apicontract.Message{}, err
	}
	if replyTo != nil {
		if err := checkIDs(*replyTo); err != nil {
			return apicontract.Message{}, err
		}
	}
	if err := CheckContent(text); err != nil {
		return apicontract.Message{}, err
	}
	var out apicontract.Message
	err := daemonclient.Call(ctx, c, http.MethodPost, "/channels/"+channelID+"/messages",
		apicontract.SendMessageJSONRequestBody{Content: text, ReplyToId: replyTo}, &out)
	return out, err
}

// PreviewInvite says where a code leads. The code goes in the body, never a path (ADR 0029).
func PreviewInvite(ctx context.Context, c daemonclient.Caller, code string) (apicontract.GuildInvitePreview, error) {
	var out apicontract.GuildInvitePreview
	err := daemonclient.Call(ctx, c, http.MethodPost, "/invites/preview",
		apicontract.GuildInviteCodeRequest{Code: code}, &out)
	return out, err
}

// JoinInvite joins the guild a code leads to and returns it. Joining one the account is already in changes
// nothing and spends no use.
func JoinInvite(ctx context.Context, c daemonclient.Caller, code string) (apicontract.Guild, error) {
	var out apicontract.Guild
	err := daemonclient.Call(ctx, c, http.MethodPost, "/invites/redeem",
		apicontract.GuildInviteCodeRequest{Code: code}, &out)
	return out, err
}

// InstanceMeta is the instance's AGPL section 13 offer: its license, and where its running version's source
// can be had. `norite about` prints it beside this build's own notice (M20a).
func InstanceMeta(ctx context.Context, c daemonclient.Caller) (apicontract.InstanceMeta, error) {
	var out apicontract.InstanceMeta
	err := daemonclient.Call(ctx, c, http.MethodGet, "/meta", nil, &out)
	return out, err
}
