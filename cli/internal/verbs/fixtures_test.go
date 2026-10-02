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
