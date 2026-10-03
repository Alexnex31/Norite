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
		t.Line("More: pass %s %s for the next page, with the same other flags.", flag, c(*next))
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

// ---------- messages ----------

type appliedTagView struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Shared    bool      `json:"shared"`
	AppliedBy string    `json:"applied_by"`
	AppliedAt time.Time `json:"applied_at"`
}

func appliedTagFrom(a apicontract.AppliedMessageTag) appliedTagView {
	return appliedTagView{ID: a.Id, Name: a.Name, Shared: a.IsShared, AppliedBy: a.AppliedBy, AppliedAt: a.AppliedAt}
}

// authorView names a message's author (M20a).
type authorView struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
}

type messageView struct {
	ID        string  `json:"id"`
	ChannelID string  `json:"channel_id"`
	AuthorID  *string `json:"author_id"`
	// Author is null for a message with no author, and for one whose author's account was deleted, which
	// keeps its author_id: the API's distinction, kept.
	Author    *authorView `json:"author"`
	Content   string      `json:"content"`
	ReplyToID *string     `json:"reply_to_id"`
	CreatedAt time.Time   `json:"created_at"`
	EditedAt  *time.Time  `json:"edited_at"`
	// Tags is null where the credential cannot read tags, and [] where the message has none: the API's
	// distinction, kept.
	Tags []appliedTagView `json:"tags"`
}

func messageFrom(m apicontract.Message) messageView {
	v := messageView{
		ID: m.Id, ChannelID: m.ChannelId, AuthorID: m.AuthorId, Content: m.Content, ReplyToID: m.ReplyToId,
		CreatedAt: m.CreatedAt, EditedAt: m.EditedAt,
	}
	if m.Author != nil {
		v.Author = &authorView{ID: m.Author.Id, Username: m.Author.Username, DisplayName: m.Author.DisplayName}
	}
	if m.Tags != nil {
		v.Tags = []appliedTagView{}
		for _, a := range *m.Tags {
			v.Tags = append(v.Tags, appliedTagFrom(a))
		}
	}
	return v
}

func (m messageView) Text(t *output.Text) {
	edited := ""
	if m.EditedAt != nil {
		edited = " (edited)"
	}
	t.Line("%s  %s  %s%s", c(m.ID), stamp(m.CreatedAt), m.byline(), edited)
	indented(t, m.Content)
	for _, tag := range m.Tags {
		t.Line("    # %s", c(tag.Name))
	}
}

// byline is who wrote the message, as a person reads it — the display name and the handle, or what stands
// in for them — followed by the author's id, which `member update` and `member remove` take. The id was the
// whole byline before M20a and dropping it for the names left a moderator nothing to act on without
// --json (/code-review). Both names are an instance's text and are sanitized (rule 19).
func (m messageView) byline() string {
	switch {
	case m.Author != nil:
		return fmt.Sprintf("%s (@%s) %s", c(m.Author.DisplayName), c(m.Author.Username), c(m.Author.ID))
	case m.AuthorID != nil:
		return "deleted account " + c(*m.AuthorID)
	}
	return "-"
}

type messagePage Page[messageView]

func (p messagePage) Text(t *output.Text) {
	if len(p.Items) == 0 {
		t.Line("No messages.")
	}
	for _, m := range p.Items {
		m.Text(t)
	}
	nextLine(t, p.Next, "--before")
}

type versionView struct {
	ID       string    `json:"id"`
	Content  string    `json:"content"`
	EditedAt time.Time `json:"edited_at"`
}

// historyView is a message's prior versions, newest first, with what it says now.
type historyView struct {
	MessageID      string        `json:"message_id"`
	ChannelID      string        `json:"channel_id"`
	AuthorID       *string       `json:"author_id"`
	CurrentContent *string       `json:"current_content"`
	EditedAt       *time.Time    `json:"edited_at"`
	DeletedAt      *time.Time    `json:"deleted_at"`
	E2E            bool          `json:"e2e"`
	Versions       []versionView `json:"versions"`
	Next           *string       `json:"next"`
}

func (h historyView) Text(t *output.Text) {
	t.Line("message %s in %s by %s", c(h.MessageID), c(h.ChannelID), output.OrNone(h.AuthorID))
	switch {
	case h.E2E:
		t.Line("  end-to-end encrypted: no version is readable here")
	case h.DeletedAt != nil:
		t.Line("  deleted %s", stamp(*h.DeletedAt))
	case h.CurrentContent != nil:
		t.Line("  now:")
		indented(t, *h.CurrentContent)
	}
	for _, v := range h.Versions {
		t.Line("  before %s:", stamp(v.EditedAt))
		indented(t, v.Content)
	}
	nextLine(t, h.Next, "--before")
}

// ---------- reports ----------

// reportView is a report as its filer or a moderator reads it. There is no reporter field, here or in any
// view below, because the API sends none: a guild moderator is never told who filed (M16).
type reportView struct {
	ID         string     `json:"id"`
	GuildID    *string    `json:"guild_id"`
	TargetType string     `json:"target_type"`
	TargetID   string     `json:"target_id"`
	Reason     string     `json:"reason"`
	Detail     *string    `json:"detail"`
	Status     string     `json:"status"`
	ResolvedBy *string    `json:"resolved_by"`
	CreatedAt  time.Time  `json:"created_at"`
	ResolvedAt *time.Time `json:"resolved_at"`
}

func reportFrom(r apicontract.Report) reportView {
	return reportView{
		ID: r.Id, GuildID: r.GuildId, TargetType: string(r.TargetType), TargetID: r.TargetId,
		Reason: string(r.ReasonCategory), Detail: r.Detail, Status: string(r.Status), ResolvedBy: r.ResolvedBy,
		CreatedAt: r.CreatedAt, ResolvedAt: r.ResolvedAt,
	}
}

func (r reportView) Text(t *output.Text) {
	t.Line("%s  %s  %s %s  %s", c(r.ID), c(r.Status), c(r.TargetType), c(r.TargetID), c(r.Reason))
	if r.Detail != nil {
		indented(t, *r.Detail)
	}
}

// triageView is a report in a moderator's queue: two facts about the target and no content, as the API
// keeps the listing.
type triageView struct {
	reportView
	TargetIsE2E     *bool      `json:"target_is_e2e"`
	TargetDeletedAt *time.Time `json:"target_deleted_at"`
}

func triageFrom(r apicontract.TriageReport) triageView {
	return triageView{
		reportView: reportView{
			ID: r.Id, GuildID: r.GuildId, TargetType: string(r.TargetType), TargetID: r.TargetId,
			Reason: string(r.ReasonCategory), Detail: r.Detail, Status: string(r.Status),
			ResolvedBy: r.ResolvedBy, CreatedAt: r.CreatedAt, ResolvedAt: r.ResolvedAt,
		},
		TargetIsE2E: r.TargetIsE2e, TargetDeletedAt: r.TargetDeletedAt,
	}
}

type triagePage Page[triageView]

func (p triagePage) Text(t *output.Text) {
	if len(p.Items) == 0 {
		t.Line("No reports.")
	}
	for _, r := range p.Items {
		r.Text(t)
	}
	nextLine(t, p.Next, "--before")
}

// triageDetailView is one report opened, with what its target says now.
type triageDetailView struct {
	triageView
	TargetChannelID *string `json:"target_channel_id"`
	TargetAuthorID  *string `json:"target_author_id"`
	TargetContent   *string `json:"target_content"`
}

func triageDetailFrom(r apicontract.TriageReportDetail) triageDetailView {
	return triageDetailView{
		triageView: triageView{
			reportView: reportView{
				ID: r.Id, GuildID: r.GuildId, TargetType: string(r.TargetType), TargetID: r.TargetId,
				Reason: string(r.ReasonCategory), Detail: r.Detail, Status: string(r.Status),
				ResolvedBy: r.ResolvedBy, CreatedAt: r.CreatedAt, ResolvedAt: r.ResolvedAt,
			},
			TargetIsE2E: r.TargetIsE2e, TargetDeletedAt: r.TargetDeletedAt,
		},
		TargetChannelID: r.TargetChannelId, TargetAuthorID: r.TargetAuthorId, TargetContent: r.TargetContent,
	}
}

func (r triageDetailView) Text(t *output.Text) {
	r.reportView.Text(t)
	t.Line("  target: %s in channel %s by %s", c(r.TargetID), output.OrNone(r.TargetChannelID),
		output.OrNone(r.TargetAuthorID))
	switch {
	case r.TargetIsE2E != nil && *r.TargetIsE2E:
		t.Line("  end-to-end encrypted: its content is not readable here")
	case r.TargetContent != nil:
		indented(t, *r.TargetContent)
	case r.TargetDeletedAt != nil:
		t.Line("  deleted %s", stamp(*r.TargetDeletedAt))
	}
}

// ---------- tags ----------

type tagView struct {
	ID        string    `json:"id"`
	GuildID   string    `json:"guild_id"`
	Name      string    `json:"name"`
	Shared    bool      `json:"shared"`
	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
}

func tagFrom(m apicontract.MessageTag) tagView {
	return tagView{
		ID: m.Id, GuildID: m.GuildId, Name: m.Name, Shared: m.IsShared, CreatedBy: m.CreatedBy,
		CreatedAt: m.CreatedAt,
	}
}

func (v tagView) Text(t *output.Text) {
	kind := "private"
	if v.Shared {
		kind = "shared"
	}
	t.Line("%s  %-7s %s", c(v.ID), kind, c(v.Name))
}

type tagList []tagView

func (l tagList) Text(t *output.Text) {
	if len(l) == 0 {
		t.Line("No tags.")
	}
	for _, v := range l {
		v.Text(t)
	}
}

type appliedTagList []appliedTagView

func (l appliedTagList) Text(t *output.Text) {
	if len(l) == 0 {
		t.Line("No tags on this message.")
	}
	for _, a := range l {
		t.Line("%s  %s  applied by %s", c(a.ID), c(a.Name), c(a.AppliedBy))
	}
}
