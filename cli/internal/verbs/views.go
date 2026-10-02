// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package verbs

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/Alexnex31/Norite/backend/apicontract"
	"github.com/Alexnex31/Norite/cli/internal/output"
)

// The views are what --json prints, one per object a verb returns, and the shapes contracts/cli-json/
// pins. They are the CLI's, not the instance's: each is re-declared and filled field by field from the
// generated contract type rather than passed through, so an instance renaming a field breaks a conversion
// here — at compile time — and not a script downstream (contracts/cli-json/README.md). Where a view and the
// API agree today, that is a fact about today.
//
// Every nullable field is present and null rather than omitted, and every list is [] rather than null:
// the two conventions the first schema set.

// ---------- guilds ----------

type guildView struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Description *string `json:"description"`
	OwnerID     string  `json:"owner_id"`
	IconHash    *string `json:"icon_hash"`
	// Recording is M16b's switch: whether the guild keeps a log of every message.
	Recording bool      `json:"recording"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func guildFrom(g apicontract.Guild) guildView {
	return guildView{
		ID: g.Id, Name: g.Name, Description: g.Description, OwnerID: g.OwnerId, IconHash: g.IconHash,
		Recording: g.MessageAuditEnabled, CreatedAt: g.CreatedAt, UpdatedAt: g.UpdatedAt,
	}
}

func (g guildView) Text(t *output.Text) {
	t.Line("%s  %s", c(g.ID), c(g.Name))
	t.Line("  owner:       %s", c(g.OwnerID))
	t.Line("  description: %s", output.OrNone(g.Description))
	t.Line("  recording:   %s", onOff(g.Recording))
	t.Line("  created:     %s", stamp(g.CreatedAt))
}

type guildList []guildView

func (l guildList) Text(t *output.Text) {
	if len(l) == 0 {
		t.Line("No guilds. Create one with `norite guild create --name NAME`.")
		return
	}
	for _, g := range l {
		t.Line("%s  %s", c(g.ID), c(g.Name))
	}
}

type auditEntryView struct {
	ID       string  `json:"id"`
	Action   string  `json:"action"`
	ActorID  string  `json:"actor_id"`
	TargetID *string `json:"target_id"`
	// Changes is the entry's diff exactly as the instance recorded it, or null.
	Changes   map[string]any `json:"changes"`
	CreatedAt time.Time      `json:"created_at"`
}

func auditEntryFrom(e apicontract.AuditLogEntry) auditEntryView {
	v := auditEntryView{
		ID: e.Id, Action: string(e.Action), ActorID: e.ActorId, TargetID: e.TargetId, CreatedAt: e.CreatedAt,
	}
	if e.Changes != nil {
		v.Changes = *e.Changes
	}
	return v
}

type auditPage Page[auditEntryView]

func (p auditPage) Text(t *output.Text) {
	if len(p.Items) == 0 {
		t.Line("No entries.")
	}
	for _, e := range p.Items {
		t.Line("%s  %s  %-22s by %s on %s", c(e.ID), stamp(e.CreatedAt), c(e.Action), c(e.ActorID),
			output.OrNone(e.TargetID))
		if len(e.Changes) > 0 {
			// The diff as JSON, sanitized: its values are names and topics people chose.
			changes, _ := json.Marshal(e.Changes)
			t.Line("    %s", output.Clean(string(changes)))
		}
	}
	nextLine(t, p.Next, "--before")
}

type recordingEntryView struct {
	ID        string    `json:"id"`
	Action    string    `json:"action"`
	ActorID   string    `json:"actor_id"`
	ChannelID string    `json:"channel_id"`
	MessageID string    `json:"message_id"`
	Content   *string   `json:"content"`
	CreatedAt time.Time `json:"created_at"`
}

func recordingEntryFrom(e apicontract.MessageAuditEntry) recordingEntryView {
	return recordingEntryView{
		ID: e.Id, Action: string(e.Action), ActorID: e.ActorId, ChannelID: e.ChannelId, MessageID: e.MessageId,
		Content: e.Content, CreatedAt: e.CreatedAt,
	}
}

type recordingPage Page[recordingEntryView]

func (p recordingPage) Text(t *output.Text) {
	if len(p.Items) == 0 {
		t.Line("No entries.")
	}
	for _, e := range p.Items {
		t.Line("%s  %s  %-6s message %s in %s by %s", c(e.ID), stamp(e.CreatedAt), c(e.Action),
			c(e.MessageID), c(e.ChannelID), c(e.ActorID))
		if e.Content != nil {
			indented(t, *e.Content)
		}
	}
	nextLine(t, p.Next, "--before")
}

// ---------- channels ----------

type overwriteView struct {
	ChannelID string `json:"channel_id"`
	TargetID  string `json:"target_id"`
	// Type is "role" or "member", where the API has 0 and 1.
	Type  string `json:"type"`
	Allow string `json:"allow"`
	Deny  string `json:"deny"`
}

func overwriteFrom(o apicontract.PermissionOverwrite) overwriteView {
	return overwriteView{
		ChannelID: o.ChannelId, TargetID: o.TargetId, Type: overwriteTypeName(int(o.Type)),
		Allow: o.Allow, Deny: o.Deny,
	}
}

func (o overwriteView) Text(t *output.Text) {
	t.Line("%s %s on channel %s: allow %s, deny %s", o.Type, c(o.TargetID), c(o.ChannelID), c(o.Allow),
		c(o.Deny))
}

type channelView struct {
	ID      string  `json:"id"`
	GuildID *string `json:"guild_id"`
	Name    *string `json:"name"`
	// Type is "text", "voice" or "category" — or "type N" for one this CLI does not know yet.
	Type          string          `json:"type"`
	ParentID      *string         `json:"parent_id"`
	Position      int             `json:"position"`
	Topic         *string         `json:"topic"`
	NSFW          bool            `json:"nsfw"`
	Bitrate       *int            `json:"bitrate"`
	UserLimit     *int            `json:"user_limit"`
	LastMessageID *string         `json:"last_message_id"`
	Overwrites    []overwriteView `json:"overwrites"`
	CreatedAt     time.Time       `json:"created_at"`
	UpdatedAt     time.Time       `json:"updated_at"`
}

func channelFrom(c apicontract.Channel) channelView {
	v := channelView{
		ID: c.Id, GuildID: c.GuildId, Name: c.Name, Type: channelTypeName(c.Type), ParentID: c.ParentId,
		Position: c.Position, Topic: c.Topic, NSFW: c.Nsfw, Bitrate: c.Bitrate, UserLimit: c.UserLimit,
		LastMessageID: c.LastMessageId, Overwrites: []overwriteView{}, CreatedAt: c.CreatedAt,
		UpdatedAt: c.UpdatedAt,
	}
	for _, o := range c.PermissionOverwrites {
		v.Overwrites = append(v.Overwrites, overwriteFrom(o))
	}
	return v
}

func (ch channelView) line() string {
	parent := ""
	if ch.ParentID != nil {
		parent = "  in " + c(*ch.ParentID)
	}
	return fmt.Sprintf("%s  %-8s %s%s", c(ch.ID), c(ch.Type), output.OrNone(ch.Name), parent)
}

func (ch channelView) Text(t *output.Text) {
	t.Line("%s", ch.line())
	t.Line("  topic:    %s", output.OrNone(ch.Topic))
	t.Line("  position: %d", ch.Position)
	for _, o := range ch.Overwrites {
		t.Line("  overwrite: %s %s allow %s deny %s", o.Type, c(o.TargetID), c(o.Allow), c(o.Deny))
	}
}

type channelList []channelView

func (l channelList) Text(t *output.Text) {
	if len(l) == 0 {
		t.Line("No channels you can see.")
	}
	for _, ch := range l {
		t.Line("%s", ch.line())
	}
}

// ---------- roles ----------

type roleView struct {
	ID          string    `json:"id"`
	GuildID     string    `json:"guild_id"`
	Name        string    `json:"name"`
	Permissions string    `json:"permissions"`
	Position    int       `json:"position"`
	Color       int       `json:"color"`
	Hoist       bool      `json:"hoist"`
	Mentionable bool      `json:"mentionable"`
	IsDefault   bool      `json:"is_default"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func roleFrom(r apicontract.Role) roleView {
	return roleView{
		ID: r.Id, GuildID: r.GuildId, Name: r.Name, Permissions: r.Permissions, Position: r.Position,
		Color: r.Color, Hoist: r.Hoist, Mentionable: r.Mentionable, IsDefault: r.IsDefault,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}

func (r roleView) Text(t *output.Text) {
	t.Line("%s  %3d  %s  permissions %s", c(r.ID), r.Position, c(r.Name), c(r.Permissions))
}

type roleList []roleView

func (l roleList) Text(t *output.Text) {
	for _, r := range l {
		r.Text(t)
	}
}

// ---------- members ----------

type memberView struct {
	GuildID  string    `json:"guild_id"`
	UserID   string    `json:"user_id"`
	Nickname *string   `json:"nickname"`
	Roles    []string  `json:"roles"`
	Mute     bool      `json:"mute"`
	Deaf     bool      `json:"deaf"`
	JoinedAt time.Time `json:"joined_at"`
}

func memberFrom(m apicontract.Member) memberView {
	roles := append([]string{}, m.Roles...)
	return memberView{
		GuildID: m.GuildId, UserID: m.UserId, Nickname: m.Nickname, Roles: roles, Mute: m.Mute, Deaf: m.Deaf,
		JoinedAt: m.JoinedAt,
	}
}

func (m memberView) Text(t *output.Text) {
	flags := ""
	if m.Mute {
		flags += " muted"
	}
	if m.Deaf {
		flags += " deafened"
	}
	t.Line("%s  %s  roles %s%s", c(m.UserID), output.OrNone(m.Nickname), c(strings.Join(m.Roles, ",")), flags)
}

type memberPage Page[memberView]

func (p memberPage) Text(t *output.Text) {
	for _, m := range p.Items {
		m.Text(t)
	}
	nextLine(t, p.Next, "--after")
}

// ---------- acknowledgements ----------

// done is what a verb prints when the instance answered with no object: what it did, and to what. M10's
// `instance invite revoke` set the precedent of saying something rather than nothing, so a script reading
// --json always has a document to parse.
type done struct {
	// Action names what happened, in the audit log's vocabulary where it has one: guild.delete,
	// member.remove, tag.apply.
	Action string `json:"action"`
	// Target is the ids the verb acted on, by the names its arguments have.
	Target map[string]string `json:"target"`
}

func (d done) Text(t *output.Text) {
	keys := make([]string, 0, len(d.Target))
	for k, v := range d.Target {
		keys = append(keys, k+" "+c(v))
	}
	slices.Sort(keys)
	t.Line("Done: %s (%s).", d.Action, strings.Join(keys, ", "))
}

// ---------- helpers ----------

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func stamp(ts time.Time) string { return ts.Local().Format("2006-01-02 15:04") }

func nextLine(t *output.Text, next *string, flag string) {
	if next != nil {
		t.Line("More: pass %s %s for the next page.", flag, c(*next))
	}
}

// indented prints content a person wrote, line by line, each line sanitized and indented so it cannot pass
// for the CLI's own output.
func indented(t *output.Text, content string) {
	for line := range strings.SplitSeq(output.Block(content), "\n") {
		t.Line("    | %s", line)
	}
}

var channelTypes = map[int]string{0: "text", 2: "voice", 4: "category"}

func channelTypeName(n int) string {
	if name, ok := channelTypes[n]; ok {
		return name
	}
	return fmt.Sprintf("type %d", n)
}

func overwriteTypeName(n int) string {
	if n == 1 {
		return "member"
	}
	return "role"
}

// c is output.Clean, short because every value an instance sent passes through it before it is printed —
// ids included, which are digits only when the instance is the one this CLI was built against (rule 19).
func c(s string) string { return output.Clean(s) }
