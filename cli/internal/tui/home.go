// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"context"
	"slices"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"github.com/Alexnex31/Norite/backend/apicontract"
	"github.com/Alexnex31/Norite/backend/gatewayproto"
	"github.com/Alexnex31/Norite/cli/internal/ops"
	"github.com/Alexnex31/Norite/daemon/ipc"
	"github.com/Alexnex31/Norite/daemon/termsafe"
)

// textChannel is the one channel type this client opens, and the one a message can be sent into (M15).
const textChannel = 0

// Home's bounds, against a hostile instance. A ListGuilds answer can name a hundred thousand guilds inside
// the relay's eight megabytes, and an instance can announce a new guild in every frame; unbounded, home
// would grow with them and ask for each one's channels. Neither bound is reached by a correct instance at
// its default ceilings, and past either home says it is showing part of the list rather than refusing it.
const (
	// maxHomeGuilds is the most guilds a server lets an account be in: the joined ceiling's own limit.
	maxHomeGuilds = gatewayproto.MaxGuilds
	// maxHomeRows bounds the channel rows across every guild: the default ceilings' product, a hundred
	// joined guilds of five hundred channels each. An instance raising both can exceed it honestly, and then
	// home lists the first fifty thousand and says so.
	maxHomeRows = 50_000
)

// keepGuild is what home holds of a guild: what it draws, bounded.
func keepGuild(g apicontract.Guild) apicontract.Guild {
	g.Name, g.Description = cut(g.Name, maxName), nil
	return g
}

// keepChannel is what home holds of a channel: what it draws and sorts by, bounded. The overwrites are the
// one field of a channel that grows with the guild, and nothing here reads them.
func keepChannel(ch apicontract.Channel) apicontract.Channel {
	if ch.Name != nil {
		name := cut(*ch.Name, maxName)
		ch.Name = &name
	}
	ch.Topic, ch.PermissionOverwrites = nil, nil
	return ch
}

// guildEntry is one guild on home, with the text channels the account can see, in position order.
type guildEntry struct {
	guild    apicontract.Guild
	channels []apicontract.Channel
}

// homeModel is home: the guilds and their channels as one list, and the redeem box.
//
// The code field always takes typing, so there is no focus to move: C-n/C-p move the list, RET opens the
// row selected — or, when the field holds a code, resolves it to a preview, and RET again joins. Joining is
// two steps, as screen `5b` draws it, so a pasted code never acts before the person has seen where it
// leads.
type homeModel struct {
	guilds   []guildEntry
	loaded   bool
	cut      bool // the instance listed more than the bounds above keep
	selected int  // index into rows()

	code    textinput.Model
	preview *previewMsg // the code resolved, while the field still holds it
	pending string      // a guild just joined, to select once home reloads
}

func newHome() homeModel {
	code := textinput.New()
	code.Prompt = "› "
	code.Placeholder = "paste an invite code"
	code.CharLimit = 64
	code.Focus()
	return homeModel{code: code}
}

func (h *homeModel) resize(width int) { h.code.SetWidth(max(width-6, 10)) }

// row is one selectable line: a channel, under its guild.
type row struct {
	guild   apicontract.Guild
	channel apicontract.Channel
}

func (h homeModel) rows() []row {
	var out []row
	for _, e := range h.guilds {
		for _, ch := range e.channels {
			out = append(out, row{guild: e.guild, channel: ch})
		}
	}
	return out
}

// ---------- keeping the list current ----------

func textChannels(chs []apicontract.Channel) []apicontract.Channel {
	var out []apicontract.Channel
	for _, ch := range chs {
		if ch.Type == textChannel && ops.IsID(ch.Id) {
			out = append(out, keepChannel(ch))
		}
	}
	slices.SortStableFunc(out, func(a, b apicontract.Channel) int {
		if a.Position != b.Position {
			return a.Position - b.Position
		}
		return compareIDs(a.Id, b.Id)
	})
	return out
}

func (h *homeModel) load(entries []guildEntry, over bool) {
	h.guilds, h.loaded, h.cut = entries, true, over
	h.bound()
	if h.pending != "" {
		for i, r := range h.rows() {
			if r.guild.Id == h.pending {
				h.selected = i
				break
			}
		}
		h.pending = ""
	}
	h.clampSelection()
}

func (h *homeModel) putGuild(e guildEntry) {
	for i := range h.guilds {
		if h.guilds[i].guild.Id == e.guild.Id {
			h.guilds[i] = e
			h.clampSelection()
			return
		}
	}
	h.guilds = append(h.guilds, e)
	h.loaded = true
	h.bound()
}

// full reports whether a guild home does not hold yet would be past maxHomeGuilds, so that it is not
// fetched at all: the requests a hostile instance can provoke are bounded with what home keeps.
func (h *homeModel) full(id string) bool {
	if _, known := h.guild(id); known || len(h.guilds) < maxHomeGuilds {
		return false
	}
	h.cut = true
	return true
}

// bound holds home to maxHomeGuilds guilds and maxHomeRows channel rows, dropping what is past either and
// noting that it did.
func (h *homeModel) bound() {
	if len(h.guilds) > maxHomeGuilds {
		h.guilds, h.cut = h.guilds[:maxHomeGuilds], true
	}
	left := maxHomeRows
	for i := range h.guilds {
		if chs := h.guilds[i].channels; len(chs) > left {
			h.guilds[i].channels, h.cut = chs[:left], true
		}
		left -= len(h.guilds[i].channels)
	}
	h.clampSelection()
}

func (h *homeModel) rename(g apicontract.Guild) bool {
	for i := range h.guilds {
		if h.guilds[i].guild.Id == g.Id {
			h.guilds[i].guild = keepGuild(g)
			return true
		}
	}
	return false
}

func (h *homeModel) dropGuild(id string) {
	h.guilds = slices.DeleteFunc(h.guilds, func(e guildEntry) bool { return e.guild.Id == id })
	h.clampSelection()
}

func (h *homeModel) putChannel(ch apicontract.Channel) {
	if ch.GuildId == nil {
		return
	}
	for i := range h.guilds {
		e := &h.guilds[i]
		if e.guild.Id != *ch.GuildId {
			continue
		}
		e.channels = slices.DeleteFunc(e.channels, func(c apicontract.Channel) bool { return c.Id == ch.Id })
		e.channels = textChannels(append(e.channels, ch))
	}
	h.bound()
}

func (h *homeModel) dropChannel(guildID, id string) {
	for i := range h.guilds {
		if h.guilds[i].guild.Id == guildID {
			h.guilds[i].channels = slices.DeleteFunc(h.guilds[i].channels,
				func(c apicontract.Channel) bool { return c.Id == id })
		}
	}
	h.clampSelection()
}

func (h homeModel) guild(id string) (apicontract.Guild, bool) {
	for _, e := range h.guilds {
		if e.guild.Id == id {
			return e.guild, true
		}
	}
	return apicontract.Guild{}, false
}

func (h homeModel) locate(channelID string) (apicontract.Guild, apicontract.Channel, bool) {
	for _, r := range h.rows() {
		if r.channel.Id == channelID {
			return r.guild, r.channel, true
		}
	}
	return apicontract.Guild{}, apicontract.Channel{}, false
}

func (h *homeModel) clampSelection() {
	n := len(h.rows())
	h.selected = max(0, min(h.selected, n-1))
}

// ---------- loading ----------

type homeMsg struct {
	gen     int
	entries []guildEntry
	over    bool // the instance listed more than home's bounds keep
	err     error
}

type guildMsg struct {
	gen   int
	entry guildEntry
	over  bool
	err   error
}

// loadHome reads the account's guilds, then each guild's channels: one request per guild over a list the
// joined ceiling bounds, kept current afterwards by the guild and channel events.
func loadHome(gen int, s Session) tea.Cmd {
	return func() tea.Msg {
		guilds, err := call(s, func(ctx context.Context, s Session) ([]apicontract.Guild, error) {
			return ops.ListGuilds(ctx, s)
		})
		if err != nil {
			return homeMsg{gen: gen, err: err}
		}
		// Bounded as it is read, not only once it is held: past maxHomeGuilds nothing more is asked for, and
		// past maxHomeRows no more channels are kept, so neither the requests nor the memory follow the
		// length of the instance's answer.
		var entries []guildEntry
		over, left := false, maxHomeRows
		for _, g := range guilds {
			if !ops.IsID(g.Id) {
				continue
			}
			if len(entries) == maxHomeGuilds {
				over = true
				break
			}
			chs, err := call(s, func(ctx context.Context, s Session) ([]apicontract.Channel, error) {
				return ops.ListChannels(ctx, s, g.Id)
			})
			if err != nil {
				return homeMsg{gen: gen, err: err}
			}
			kept := textChannels(chs)
			if len(kept) > left {
				kept, over = kept[:left], true
			}
			left -= len(kept)
			entries = append(entries, guildEntry{guild: keepGuild(g), channels: kept})
		}
		return homeMsg{gen: gen, entries: entries, over: over}
	}
}

func loadGuild(gen int, s Session, g apicontract.Guild) tea.Cmd {
	if s == nil {
		return nil
	}
	return func() tea.Msg {
		chs, err := call(s, func(ctx context.Context, s Session) ([]apicontract.Channel, error) {
			return ops.ListChannels(ctx, s, g.Id)
		})
		kept, over := textChannels(chs), false
		if len(kept) > maxHomeRows {
			kept, over = kept[:maxHomeRows], true // the rest of home's bound is bound()'s, once it is held
		}
		return guildMsg{gen: gen, entry: guildEntry{guild: keepGuild(g), channels: kept}, over: over, err: err}
	}
}

// ---------- the redeem box ----------

type previewMsg struct {
	code    string
	preview apicontract.GuildInvitePreview
	err     error
}

type joinedMsg struct {
	guild apicontract.Guild
	err   error
}

func (h *homeModel) codeChanged() {
	if h.preview != nil && h.preview.code != strings.TrimSpace(h.code.Value()) {
		h.preview = nil
	}
}

func (h *homeModel) previewed(msg previewMsg) {
	if msg.err != nil || msg.code != strings.TrimSpace(h.code.Value()) {
		h.preview = nil
		return
	}
	h.preview = &msg
}

func (h *homeModel) joined(guildID string) {
	h.code.Reset()
	h.preview = nil
	h.pending = guildID
}

func (m Model) homeKey(k tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	h := &m.home
	switch k.Keystroke() {
	case "ctrl+n", "down":
		h.selected = min(h.selected+1, max(len(h.rows())-1, 0))
		return m, nil
	case "ctrl+p", "up":
		h.selected = max(h.selected-1, 0)
		return m, nil
	case "esc":
		h.code.Reset()
		h.preview = nil
		m.setStatus("", false)
		return m, nil
	case "enter":
		code := strings.TrimSpace(h.code.Value())
		if code != "" {
			if m.sess == nil {
				m.setStatus("not attached to the daemon yet", true)
				return m, nil
			}
			s := m.sess
			if h.preview != nil && h.preview.code == code {
				return m, func() tea.Msg {
					g, err := call(s, func(ctx context.Context, s Session) (apicontract.Guild, error) {
						return ops.JoinInvite(ctx, s, code)
					})
					return joinedMsg{guild: g, err: err}
				}
			}
			m.setStatus("looking up that invite", false)
			return m, func() tea.Msg {
				p, err := call(s, func(ctx context.Context, s Session) (apicontract.GuildInvitePreview, error) {
					return ops.PreviewInvite(ctx, s, code)
				})
				return previewMsg{code: code, preview: p, err: err}
			}
		}
		rows := h.rows()
		if len(rows) == 0 {
			return m, nil
		}
		r := rows[h.selected]
		m.pane = newPane(r.guild.Id, r.channel.Id)
		m.pane.guildName, m.pane.channelName = r.guild.Name, deref(r.channel.Name)
		m.pane.resize(m.width)
		m.setStatus("", false)
		if m.sess == nil {
			return m, nil
		}
		return m, m.pane.fetch(m.gen, m.sess)
	}
	var cmd tea.Cmd
	h.code, cmd = h.code.Update(k)
	h.codeChanged()
	return m, cmd
}

// ---------- drawing ----------

func (h homeModel) view(account *ipc.Account, width, height int) string {
	var top []string
	who := "not signed in"
	if account != nil {
		// Read from a file a person can edit, and sanitized there; sanitized again, since it is drawn.
		who = "signed in as @" + termsafe.Text(account.Username)
	}
	top = append(top, clip(sBold.Render("Norite")+sDim.Render(" · "+who), width), "")

	// The redeem box, drawn below the list but sized first, since the list takes whatever it leaves.
	box := []string{sLabel.Render("REDEEM AN INVITE"), "  " + h.code.View()}
	if p := h.preview; p != nil {
		box = append(box, clip("  "+sAccent.Render("✓ ")+previewLine(p.preview)+sDim.Render(" · RET join"), width))
	} else {
		box = append(box, sDim.Render("  RET looks the code up; RET again joins"))
	}

	list := []string{sLabel.Render("YOUR GUILDS")}
	avail := height - len(top) - len(box) - 2
	switch rows := h.rows(); {
	case !h.loaded:
		list = append(list, sDim.Render("  loading…"))
	case len(rows) == 0:
		list = append(list, "  "+sBold.Render("NO GUILDS YET"),
			sDim.Render("  Redeem an invite below, or start one with `norite guild create --name NAME`."))
	case h.cut:
		list = append(list, h.listLines(rows, width, avail-2)...)
		list = append(list, clip(sWarn.Render("  the instance listed more than this client keeps; not all are shown"),
			width))
	default:
		list = append(list, h.listLines(rows, width, avail-1)...)
	}

	lines := append(append(top, list...), "")
	lines = append(lines, box...)
	return fit(lines, height)
}

// listLines draws the guilds and their channels, scrolled so the selected row is in view.
func (h homeModel) listLines(rows []row, width, avail int) []string {
	type line struct {
		text string
		row  int // -1 for a guild's heading
	}
	var all []line
	last := ""
	for i, r := range rows {
		if r.guild.Id != last {
			all = append(all, line{text: "  " + sBold.Render(termsafe.Text(r.guild.Name)), row: -1})
			last = r.guild.Id
		}
		name := "# " + termsafe.Text(deref(r.channel.Name))
		if i == h.selected {
			all = append(all, line{text: "  " + sSelect.Render("› "+name), row: i})
		} else {
			all = append(all, line{text: "    " + name, row: i})
		}
	}
	at := 0
	for i, l := range all {
		if l.row == h.selected {
			at = i
		}
	}
	start := 0
	if avail > 0 && at >= avail {
		start = at - avail + 1
	}
	var out []string
	for i := start; i < len(all) && (avail <= 0 || len(out) < avail); i++ {
		out = append(out, clip(all[i].text, width))
	}
	return out
}

// previewLine is `5b`'s resolved-preview row: guild, inviter, expiry. Every value is the instance's, and a
// stranger's at that, until the person decides to join.
func previewLine(p apicontract.GuildInvitePreview) string {
	parts := []string{sBold.Render(termsafe.Text(p.Guild.Name)), "#" + termsafe.Text(deref(p.Channel.Name))}
	if p.Inviter != nil {
		parts = append(parts, "invited by "+termsafe.Text(p.Inviter.DisplayName)+
			" (@"+termsafe.Text(p.Inviter.Username)+")")
	}
	if p.ExpiresAt != nil {
		parts = append(parts, "expires "+p.ExpiresAt.Local().Format("2006-01-02 15:04"))
	} else {
		parts = append(parts, "never expires")
	}
	return strings.Join(parts, sDim.Render(" · "))
}
