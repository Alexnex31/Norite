// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"context"
	"slices"
	"strings"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/Alexnex31/Norite/backend/apicontract"
	"github.com/Alexnex31/Norite/cli/internal/ops"
	"github.com/Alexnex31/Norite/daemon/termsafe"
)

// maxHeld is how many messages a pane keeps, the oldest dropped first: a bound on a client's memory that
// does not depend on how busy a channel is.
const maxHeld = 500

// historyPage is how much backlog a pane reads when it opens: one page, through the relay, as `norite
// message list` reads it. Reading further back is M43's scrollback, with the daemon's buffer.
const historyPage = 50

// composerRows bounds how tall the composer grows with a multi-line message before it scrolls.
const composerRows = 5

// paneModel is one open channel: what has been said in it, and the composer.
type paneModel struct {
	guildID, channelID     string
	guildName, channelName string

	msgs   []apicontract.Message // ascending by id, at most maxHeld
	loaded bool
	gone   string // why the channel can no longer be used, once it cannot

	scroll   int // rows above the bottom; 0 follows new messages
	composer textarea.Model
	sending  bool
}

func newPane(guildID, channelID string) *paneModel {
	c := textarea.New()
	c.Prompt = "› "
	c.Placeholder = "write a message"
	c.ShowLineNumbers = false
	c.DynamicHeight = true
	c.MinHeight, c.MaxHeight = 1, composerRows
	// Enter sends; it does not break the line. A paste keeps its own line breaks, which arrive as a
	// PasteMsg rather than as Enter.
	c.KeyMap.InsertNewline.SetEnabled(false)
	c.Focus()
	return &paneModel{guildID: guildID, channelID: channelID, composer: c}
}

func (p *paneModel) resize(width int) { p.composer.SetWidth(max(width, 10)) }

// ---------- what has been said ----------

type historyMsg struct {
	gen       int
	channelID string
	msgs      []apicontract.Message
	err       error
}

type sentMsg struct {
	channelID string
	msg       apicontract.Message
	err       error
}

// fetch reads one page of the channel's backlog.
//
// The client is already attached for events when this runs, so a message arriving while the page is being
// read is applied as it arrives and the page is merged around it by id: nothing that arrived in between is
// lost, and nothing in both is shown twice.
func (p *paneModel) fetch(gen int, s Session) tea.Cmd {
	channel := p.channelID
	return func() tea.Msg {
		page, err := call(s, func(ctx context.Context, s Session) ([]apicontract.Message, error) {
			return ops.ListMessages(ctx, s, channel, ops.Page{Limit: historyPage})
		})
		return historyMsg{gen: gen, channelID: channel, msgs: page, err: err}
	}
}

// hold is what the pane keeps of a message: what it draws, each part bounded by what a correct instance can
// send. maxHeld counts messages, and without this a hostile instance makes each one as large as a frame —
// four megabytes on the gateway, so two gigabytes for a full pane, re-wrapped on every keystroke. Kept, a
// message is at most its 4,000 runes and two names.
func hold(m apicontract.Message) apicontract.Message {
	m.Content = cut(m.Content, ops.MaxContent)
	if m.Author != nil {
		a := *m.Author
		a.Id, a.Username, a.DisplayName = cut(a.Id, 20), cut(a.Username, maxName), cut(a.DisplayName, maxName)
		m.Author = &a
	}
	if m.AuthorId != nil {
		id := cut(*m.AuthorId, 20) // only its presence is read: it tells a deleted account from a system message
		m.AuthorId = &id
	}
	m.ReplyToId, m.Tags = nil, nil // neither is drawn yet
	return m
}

// merge adds a page, replacing any message it already holds by id.
func (p *paneModel) merge(page []apicontract.Message) {
	for _, m := range page {
		if m.ChannelId == p.channelID {
			p.upsert(m)
		}
	}
	p.loaded = true
	p.trim()
}

// put adds or replaces one message as it arrives. Scrolled up, the view stays where it was rather than
// following: new lines raise the offset from the bottom by exactly what they add.
func (p *paneModel) put(m apicontract.Message, width int) {
	if m.ChannelId != p.channelID || !ops.IsID(m.Id) {
		return
	}
	// Measured on the one message, not by laying out the pane twice: 10 ms per arrival with a full pane of
	// 400-character messages, against one message's wrap.
	if p.scroll > 0 {
		if old, found := p.find(m.Id); found {
			p.scroll -= len(messageLines(old, width))
		}
		p.scroll = max(p.scroll+len(messageLines(hold(m), width)), 0)
	}
	p.upsert(m)
	p.trim()
}

// find returns the held message with this id.
func (p *paneModel) find(id string) (apicontract.Message, bool) {
	i, found := slices.BinarySearchFunc(p.msgs, id, func(have apicontract.Message, id string) int {
		return compareIDs(have.Id, id)
	})
	if !found {
		return apicontract.Message{}, false
	}
	return p.msgs[i], true
}

func (p *paneModel) upsert(m apicontract.Message) {
	if !ops.IsID(m.Id) {
		return
	}
	m = hold(m)
	i, found := slices.BinarySearchFunc(p.msgs, m.Id, func(have apicontract.Message, id string) int {
		return compareIDs(have.Id, id)
	})
	if found {
		p.msgs[i] = m
		return
	}
	p.msgs = slices.Insert(p.msgs, i, m)
}

func (p *paneModel) trim() {
	if over := len(p.msgs) - maxHeld; over > 0 {
		p.msgs = slices.Delete(p.msgs, 0, over)
	}
}

func (p *paneModel) remove(id string) {
	p.msgs = slices.DeleteFunc(p.msgs, func(m apicontract.Message) bool { return m.Id == id })
}

// ---------- keys ----------

func (m Model) paneKey(k tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	p := m.pane
	area := m.messageRows()
	switch k.Keystroke() {
	case "pgup":
		step := max(area-1, 1)
		p.scroll = min(p.scroll+step, max(len(p.tail(m.width, p.scroll+step+area))-area, 0))
		return m, nil
	case "pgdown":
		p.scroll = max(p.scroll-max(area-1, 1), 0)
		return m, nil
	case "enter":
		if p.gone != "" {
			m.setStatus(p.gone+"; ESC goes home", true)
			return m, nil
		}
		if p.sending {
			return m, nil
		}
		text := p.composer.Value()
		if err := ops.CheckContent(text); err != nil {
			m.setStatus(err.Error(), true)
			return m, nil
		}
		if m.sess == nil {
			m.setStatus("not attached to the daemon; the message is kept", true)
			return m, nil
		}
		p.sending = true
		s, channel := m.sess, p.channelID
		return m, func() tea.Msg {
			sent, err := call(s, func(ctx context.Context, s Session) (apicontract.Message, error) {
				return ops.SendMessage(ctx, s, channel, text, nil)
			})
			return sentMsg{channelID: channel, msg: sent, err: err}
		}
	}
	if p.gone != "" {
		return m, nil
	}
	var cmd tea.Cmd
	p.composer, cmd = p.composer.Update(k)
	return m, cmd
}

// ---------- drawing ----------

// messageRows is how many rows the message list has: the pane less its header, the rule, the composer and
// the hint row.
func (m Model) messageRows() int {
	return max(m.height-1-1-1-m.pane.composerHeight(), 1)
}

func (p *paneModel) composerHeight() int {
	if p.gone != "" {
		return 1
	}
	return max(min(p.composer.Height(), composerRows), 1)
}

// tail lays out the newest held messages, oldest first, as many as fill at least `rows` rows of the given
// width: each a header row — the author, the time, whether it was edited — and its content beneath,
// wrapped by display width and indented. A frame shows the bottom of the pane, so laying out all 500 to
// draw thirty rows cost 12 ms a frame at 400-character messages and 59 ms at the 4,000-rune limit, on every
// keystroke in the composer.
func (p *paneModel) tail(width, rows int) []string {
	var chunks [][]string
	n := 0
	for i := len(p.msgs) - 1; i >= 0 && n < rows; i-- {
		l := messageLines(p.msgs[i], width)
		chunks = append(chunks, l)
		n += len(l)
	}
	out := make([]string, 0, n)
	for i := len(chunks) - 1; i >= 0; i-- {
		out = append(out, chunks[i]...)
	}
	return out
}

func messageLines(msg apicontract.Message, width int) []string {
	header := byline(msg) + "  " + sDim.Render(msg.CreatedAt.Local().Format("15:04"))
	if msg.EditedAt != nil {
		header += sDim.Render("  (edited)")
	}
	lines := []string{clip(header, width)}

	// Type 0 is a message a person typed and 1 one sent through automation (M22); anything else is a kind
	// this client does not draw, and says so rather than guessing.
	if msg.Type != 0 && msg.Type != 1 {
		return append(lines, "  "+sDim.Render("(a kind of message this client cannot show)"))
	}
	// Plain text: rule 9 is met by interpreting no markup, and rule 19 by termsafe, which keeps line breaks
	// and tabs and removes everything a terminal would act on. Tabs become spaces so wrapping can count them.
	content := strings.ReplaceAll(termsafe.Block(msg.Content), "\t", "    ")
	for _, para := range strings.Split(content, "\n") {
		wrapped := lipgloss.Wrap(para, max(width-2, 1), " ")
		for _, l := range strings.Split(wrapped, "\n") {
			lines = append(lines, "  "+l)
		}
	}
	return lines
}

// byline names who wrote a message: the display name, a deleted account, or nobody for a system message.
func byline(msg apicontract.Message) string {
	switch {
	case msg.Author != nil:
		return sBold.Render(termsafe.Text(msg.Author.DisplayName))
	case msg.AuthorId != nil:
		return sDim.Render("deleted account")
	}
	return sDim.Render("system")
}

func (p *paneModel) view(width, height int) string {
	name := "# " + termsafe.Text(p.channelName)
	if p.channelName == "" {
		name = "# " + p.channelID
	}
	if p.guildName != "" {
		name += sDim.Render(" · " + termsafe.Text(p.guildName))
	}
	out := []string{clip(sBold.Render(name), width)}

	area := max(height-1-1-p.composerHeight(), 1)
	var body []string
	switch all := p.tail(width, p.scroll+area); {
	case !p.loaded:
		body = []string{sDim.Render("loading…")}
	case len(p.msgs) == 0:
		body = []string{sDim.Render("No messages yet. Say something.")}
	default:
		end := max(len(all)-p.scroll, 0)
		start := max(end-area, 0)
		body = all[start:end]
	}
	// Bottom-aligned: the newest message sits on the row above the rule, as a chat does.
	for len(body) < area {
		body = append([]string{""}, body...)
	}
	out = append(out, body...)
	out = append(out, sDim.Render(strings.Repeat("─", max(width, 1))))
	if p.gone != "" {
		out = append(out, clip(sDanger.Render(p.gone)+sDim.Render(" · ESC home"), width))
	} else {
		out = append(out, p.composer.View())
	}
	return fit(out, height)
}
