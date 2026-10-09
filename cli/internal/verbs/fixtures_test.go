// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package verbs

import (
	"net/http"
	"time"

	"github.com/Alexnex31/Norite/backend/apicontract"
)

// Payloads the contract accepts, for the fake daemon to answer with. It validates every one, so a fixture
// missing a field the instance always sends fails the test that uses it.

var at = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

func ptr[T any](v T) *T { return &v }

func apiGuild(id, name string) apicontract.Guild {
	return apicontract.Guild{Id: id, Name: name, OwnerId: "1", CreatedAt: at, UpdatedAt: at}
}

func apiChannel(id, guild, name string, typ int) apicontract.Channel {
	return apicontract.Channel{
		Id: id, GuildId: &guild, Name: &name, Type: typ, CreatedAt: at, UpdatedAt: at,
		PermissionOverwrites: []apicontract.PermissionOverwrite{},
	}
}

func apiRole(id, guild, name string, position int) apicontract.Role {
	return apicontract.Role{
		Id: id, GuildId: guild, Name: name, Permissions: "1024", Position: position, CreatedAt: at, UpdatedAt: at,
	}
}

func apiMember(guild, user string, roles ...string) apicontract.Member {
	if roles == nil {
		roles = []string{}
	}
	return apicontract.Member{GuildId: guild, UserId: user, Roles: roles, JoinedAt: at}
}

func apiOverwrite(channel, target string, typ int, allow, deny string) apicontract.PermissionOverwrite {
	return apicontract.PermissionOverwrite{
		ChannelId: channel, TargetId: target, Type: apicontract.PermissionOverwriteType(typ),
		Allow: allow, Deny: deny,
	}
}

func apiAudit(id, action string) apicontract.AuditLogEntry {
	return apicontract.AuditLogEntry{
		Id: id, Action: apicontract.AuditLogAction(action), ActorId: "1", TargetId: ptr("10"), CreatedAt: at,
		Changes: &map[string]interface{}{"name": map[string]any{"from": "Old", "to": "New"}},
	}
}

func apiRecorded(id, action, content string) apicontract.MessageAuditEntry {
	return apicontract.MessageAuditEntry{
		Id: id, Action: apicontract.MessageAuditAction(action), ActorId: "1", ChannelId: "20", MessageId: "30",
		Content: &content, CreatedAt: at,
	}
}

// refusal is the instance's error envelope.
func refusal(code, message string) map[string]any {
	return map[string]any{"error": map[string]any{"code": code, "message": message, "request_id": "req-1"}}
}

func ok(body any) answerFunc { return func(request) (int, any) { return http.StatusOK, body } }

func created(body any) answerFunc {
	return func(request) (int, any) { return http.StatusCreated, body }
}

func noContent() answerFunc { return func(request) (int, any) { return http.StatusNoContent, nil } }

func apiMessage(id, channel, content string) apicontract.Message {
	return apicontract.Message{
		Id: id, ChannelId: channel, AuthorId: ptr("1"), Content: content, CreatedAt: at,
		Author: &apicontract.PublicUser{Id: "1", Username: "alice", DisplayName: "Alice"},
		Tags:   &[]apicontract.AppliedMessageTag{},
	}
}

func apiHistory(message, channel string, versions ...string) apicontract.MessageEditHistory {
	h := apicontract.MessageEditHistory{
		MessageId: message, ChannelId: channel, AuthorId: ptr("2"), CurrentContent: ptr("now"), EditedAt: &at,
		Versions: []apicontract.MessageEditVersion{},
	}
	for i, v := range versions {
		h.Versions = append(h.Versions, apicontract.MessageEditVersion{Id: "9" + string(rune('0'+i)), Content: v, EditedAt: at})
	}
	return h
}

func apiReport(id, status string) apicontract.Report {
	return apicontract.Report{
		Id: id, GuildId: ptr("10"), TargetType: apicontract.ReportTargetTypeMessage, TargetId: "30",
		ReasonCategory: "spam", Detail: ptr("look"), Status: apicontract.ReportStatus(status), CreatedAt: at,
	}
}

func apiTriage(id string) apicontract.TriageReport {
	return apicontract.TriageReport{
		Id: id, GuildId: ptr("10"), TargetType: apicontract.ReportTargetTypeMessage, TargetId: "30",
		ReasonCategory: "spam", Detail: ptr("look"), Status: "open", CreatedAt: at, TargetIsE2e: ptr(false),
	}
}

func apiTriageDetail(id string) apicontract.TriageReportDetail {
	return apicontract.TriageReportDetail{
		Id: id, GuildId: ptr("10"), TargetType: apicontract.ReportTargetTypeMessage, TargetId: "30",
		ReasonCategory: "spam", Detail: ptr("look"), Status: "open", CreatedAt: at, TargetIsE2e: ptr(false),
		TargetChannelId: ptr("20"), TargetAuthorId: ptr("2"), TargetContent: ptr("the reported text"),
	}
}

func apiTag(id, name string, shared bool) apicontract.MessageTag {
	return apicontract.MessageTag{Id: id, GuildId: "10", Name: name, IsShared: shared, CreatedBy: "1", CreatedAt: at}
}

func apiApplied(id, name string) apicontract.AppliedMessageTag {
	return apicontract.AppliedMessageTag{
		Id: id, GuildId: "10", Name: name, CreatedBy: "1", CreatedAt: at, AppliedBy: "1", AppliedAt: at,
	}
}

func apiInvite(id, code string) apicontract.GuildInvite {
	five := 5
	expiry := at.Add(7 * 24 * time.Hour)
	return apicontract.GuildInvite{
		Id: id, Code: code, GuildId: "10", ChannelId: "20", InviterId: "1",
		Inviter: &apicontract.PublicUser{Id: "1", Username: "alice", DisplayName: "Alice"},
		MaxUses: &five, ExpiresAt: &expiry, CreatedAt: at,
	}
}

func apiPreview(code, guildName string) apicontract.GuildInvitePreview {
	p := apicontract.GuildInvitePreview{
		Code:    code,
		Inviter: &apicontract.PublicUser{Id: "1", Username: "alice", DisplayName: "Alice"},
	}
	p.Guild.Id, p.Guild.Name, p.Guild.Description = "10", guildName, ptr("a place to talk")
	p.Channel.Id, p.Channel.Name = "20", ptr("general")
	return p
}

// tokenValue is shaped like a real one and announces that it is not: 43 characters after the prefix.
const tokenValue = "nat_" + "EXAMPLEexampleEXAMPLEexampleEXAMPLEexample0"

func apiToken(id, name string) apicontract.ApiToken {
	return apicontract.ApiToken{
		Id: id, Name: name, Scopes: []apicontract.Scope{apicontract.MessagesWrite}, CreatedAt: at,
	}
}

func apiMinted(id, name string) apicontract.MintedApiToken {
	return apicontract.MintedApiToken{
		Id: id, Name: name, Scopes: []apicontract.Scope{apicontract.MessagesWrite}, CreatedAt: at, Value: tokenValue,
	}
}
