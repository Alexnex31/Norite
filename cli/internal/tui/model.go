// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/Alexnex31/Norite/backend/apicontract"
	"github.com/Alexnex31/Norite/cli/internal/ops"
	"github.com/Alexnex31/Norite/daemon/ipc"
	"github.com/Alexnex31/Norite/daemon/termsafe"
)

// Minimum size, from docs/design/tui/README.md's grid: below it the client draws one line saying so.
const (
	minWidth  = 40
	minHeight = 12
)

// Waiting for the daemon: how long one attach may take, and how the retries after a failed one space out.
const (
	dialTimeout = 10 * time.Second
	retryFirst  = time.Second
	retryMost   = 8 * time.Second
	callTimeout = 30 * time.Second
)

// Model is the client: the attachment, home, and the pane when a channel is open.
type Model struct {
	opts          Options
	width, height int

	// gen numbers attachments. Everything a command brings back carries the gen it was started under, and
	// an answer from an attachment that has since ended is dropped rather than applied to the next one.
	gen     int
	sess    Session
	account *ipc.Account
	retry   time.Duration

	home homeModel
	pane *paneModel

	// status is the one line the hint row shows instead of its keys: what just went wrong, or what the
	// client is waiting for. statusErr colors it as an error.
	status    string
	statusErr bool

	armed bool // C-x has been pressed, and the next key completes the chord
}

// New builds the client. Nothing is attached until Init.
func New(opts Options) Model {
	// Generation 1 is the attachment Init starts. Init has a value receiver, so it cannot advance the count
	// itself; every later attachment advances it first (redial).
	m := Model{opts: opts, home: newHome(), gen: 1}
	if opts.Channel != "" {
		m.pane = newPane("", opts.Channel)
	}
	return m
}

// Init attaches to the daemon.
func (m Model) Init() tea.Cmd {
	return dial(m.gen, m.opts.Dial)
}

// ---------- messages a command brings back ----------

type attachedMsg struct {
	gen  int
	sess Session
	err  error
}

type eventMsg struct {
	gen int
	ev  ipc.Event
}

type endedMsg struct {
	gen int
	err error
}

type retryMsg struct{ gen int }

// ---------- the attachment ----------

// redial starts a new attachment, numbered past every earlier one so their answers are dropped.
func (m *Model) redial() tea.Cmd {
	m.gen++
	return dial(m.gen, m.opts.Dial)
}

func dial(gen int, dial Dialer) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), dialTimeout)
		defer cancel()
		s, err := dial(ctx)
		return attachedMsg{gen: gen, sess: s, err: err}
	}
}

// listen waits for the attachment's next event, or for its end.
func listen(gen int, s Session) tea.Cmd {
	return func() tea.Msg {
		ev, open := <-s.Events()
		if !open {
			return endedMsg{gen: gen, err: s.Err()}
		}
		return eventMsg{gen: gen, ev: ev}
	}
}

// call runs one of ops' functions against the attachment, bounded so a daemon that stops answering costs a
// status line rather than a client that waits for ever.
func call[T any](s Session, op func(ctx context.Context, s Session) (T, error)) (T, error) {
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	return op(ctx, s)
}

func (m *Model) backOff(why string) tea.Cmd {
	if m.retry == 0 {
		m.retry = retryFirst
	} else {
		m.retry = min(m.retry*2, retryMost)
	}
	// When comes first: a dial error carries a socket path, and a row cut to the terminal's width should
	// lose the detail rather than the one thing the person is waiting to learn.
	m.setStatus(fmt.Sprintf("trying again in %s · %s", m.retry, why), true)
	gen := m.gen
	return tea.Tick(m.retry, func(time.Time) tea.Msg { return retryMsg{gen: gen} })
}

func (m *Model) onAttached(msg attachedMsg) tea.Cmd {
	if msg.gen != m.gen {
		if msg.sess != nil {
			_ = msg.sess.Close()
		}
		return nil
	}
	if msg.err != nil {
		return m.backOff(termsafe.Text(msg.err.Error()))
	}
	m.sess, m.retry = msg.sess, 0
	cmds := []tea.Cmd{listen(m.gen, m.sess)}

	ready := m.sess.Ready()
	switch ready.Standing {
	case ipc.StandingSignedIn:
		if m.account != nil && !sameAccount(m.account, ready.Account) {
			m.forget()
		}
		m.account = ready.Account
		m.setStatus("", false)
		cmds = append(cmds, loadHome(m.gen, m.sess))
		if m.pane != nil {
			// Read afresh rather than merged into what the pane held: a resync means the daemon's view was
			// cleared, and a message deleted in the gap would otherwise stay on screen as history.
			m.pane.msgs, m.pane.loaded, m.pane.unread, m.pane.scroll = nil, false, false, 0
			cmds = append(cmds, m.pane.fetch(m.gen, m.sess))
		}
	case ipc.StandingStarting:
		// The daemon closes this attachment with resync once its own session is ready, and the client
		// attaches again then: nothing to poll.
		m.setStatus("the daemon is starting; waiting for it", false)
	default:
		// The same: a login starts the daemon a fresh session, which resyncs every watching client.
		m.forget()
		m.account = nil
		m.setStatus("signed out; run `norite login`, and this will carry on", true)
	}
	return tea.Batch(cmds...)
}

// forget drops everything drawn for the sign-in that ended: home's guilds and the open pane, its draft
// included. The daemon forgets on a sign-in's end and closes this client so it resyncs (M19, M20); a client
// that went on drawing the old account's conversation after a logout, or under the next account's name,
// would undo that on the one screen it matters. Only what the code box holds survives, being the person's.
func (m *Model) forget() {
	m.home.forget()
	m.pane = nil
}

// sameAccount reports whether two sign-ins name one account on one instance.
func sameAccount(a, b *ipc.Account) bool {
	return a != nil && b != nil && a.UserID == b.UserID && a.InstanceURL == b.InstanceURL
}

// paneGone stops the open pane's composer, saying why, and clears a status line about the pane as it was:
// a refused send's error describes nothing the person can still act on.
func (m *Model) paneGone(why string) {
	m.pane.gone = why
	m.setStatus("", false)
}

func (m *Model) onEnded(msg endedMsg) tea.Cmd {
	if msg.gen != m.gen {
		return nil
	}
	m.sess = nil
	var ce *ipc.CloseError
	if errors.As(msg.err, &ce) && ce.Code == ipc.CloseResync {
		// The state this client was built from was cleared: attach again and fetch everything afresh, the
		// open channel's page included, rather than show the gap as history.
		m.setStatus("resynchronizing with the daemon", false)
		return m.redial()
	}
	why := "the daemon is unavailable"
	if errors.As(msg.err, &ce) && ce.Code == ipc.CloseGoingAway {
		why = "the daemon is stopping"
	}
	return m.backOff(why)
}

// ---------- events ----------

func (m *Model) onEvent(ev ipc.Event) tea.Cmd {
	switch ev.Type {
	case "MESSAGE_CREATE", "MESSAGE_UPDATE":
		var msg apicontract.Message
		if json.Unmarshal(ev.Data, &msg) == nil && m.pane != nil && m.pane.channelID == msg.ChannelId {
			m.pane.put(msg, m.width)
		}
	case "MESSAGE_DELETE":
		var d struct {
			ID        string `json:"id"`
			ChannelID string `json:"channel_id"`
		}
		if json.Unmarshal(ev.Data, &d) == nil && m.pane != nil && m.pane.channelID == d.ChannelID {
			m.pane.remove(d.ID)
		}
	case "GUILD_CREATE", "GUILD_UPDATE":
		var g apicontract.Guild
		if json.Unmarshal(ev.Data, &g) == nil && ops.IsID(g.Id) {
			if ev.Type == "GUILD_UPDATE" && m.home.rename(g) {
				m.refreshPaneNames()
				return nil
			}
			if m.home.full(g.Id) {
				return nil
			}
			return loadGuild(m.gen, m.sess, g)
		}
	case "GUILD_DELETE":
		var d struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(ev.Data, &d) == nil {
			m.home.dropGuild(d.ID)
			if m.pane != nil && m.pane.guildID == d.ID {
				m.paneGone("this guild was deleted, or you are no longer in it")
			}
		}
	case "CHANNEL_CREATE", "CHANNEL_UPDATE":
		var ch apicontract.Channel
		if json.Unmarshal(ev.Data, &ch) == nil {
			m.home.putChannel(ch)
			m.refreshPaneNames()
		}
	case "CHANNEL_DELETE":
		var d struct {
			ID      string `json:"id"`
			GuildID string `json:"guild_id"`
		}
		if json.Unmarshal(ev.Data, &d) == nil {
			m.home.dropChannel(d.GuildID, d.ID)
			if m.pane != nil && m.pane.channelID == d.ID {
				m.paneGone("this channel was deleted")
			}
		}
	case "GUILD_PERMISSIONS_UPDATE":
		// Which channels may be seen may have changed: the contract says to refetch the guild's channels.
		var d struct {
			GuildID string `json:"guild_id"`
		}
		if json.Unmarshal(ev.Data, &d) == nil {
			if g, found := m.home.guild(d.GuildID); found {
				return loadGuild(m.gen, m.sess, g)
			}
		}
	}
	return nil
}

// refreshPaneNames gives the open pane the names home now knows, which a pane opened by --channel has none
// of until home loads.
func (m *Model) refreshPaneNames() {
	if m.pane == nil {
		return
	}
	if g, ch, found := m.home.locate(m.pane.channelID); found {
		m.pane.guildID, m.pane.guildName, m.pane.channelName = g.Id, g.Name, deref(ch.Name)
	}
}

// ---------- update ----------

func (m *Model) setStatus(s string, isErr bool) { m.status, m.statusErr = s, isErr }

// Update handles one message.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.home.resize(m.width)
		if m.pane != nil {
			m.pane.resize(m.width)
		}
		return m, nil

	case attachedMsg:
		return m, m.onAttached(msg)
	case endedMsg:
		return m, m.onEnded(msg)
	case retryMsg:
		if msg.gen == m.gen && m.sess == nil {
			return m, m.redial()
		}
		return m, nil
	case eventMsg:
		if msg.gen != m.gen || m.sess == nil {
			return m, nil
		}
		cmd := m.onEvent(msg.ev)
		return m, tea.Batch(cmd, listen(m.gen, m.sess))

	case homeMsg:
		if msg.gen != m.gen {
			return m, nil
		}
		if msg.err != nil {
			m.setStatus("could not load your guilds: "+termsafe.Text(msg.err.Error()), true)
			return m, nil
		}
		m.home.load(msg.entries, msg.over)
		m.refreshPaneNames()
		return m, nil
	case guildMsg:
		if msg.gen != m.gen || msg.err != nil {
			return m, nil
		}
		m.home.putGuild(msg.entry)
		m.home.cut = m.home.cut || msg.over
		m.refreshPaneNames()
		if m.pane != nil && m.pane.guildID == msg.entry.guild.Id {
			if _, _, found := m.home.locate(m.pane.channelID); !found {
				m.paneGone("you can no longer see this channel")
			}
		}
		return m, nil
	case historyMsg:
		if msg.gen != m.gen || m.pane == nil || m.pane.channelID != msg.channelID {
			return m, nil
		}
		if msg.err != nil {
			m.setStatus("could not read this channel: "+termsafe.Text(msg.err.Error()), true)
			m.pane.loaded, m.pane.unread = true, true
			return m, nil
		}
		m.pane.merge(msg.msgs)
		return m, nil
	case sentMsg:
		if m.pane == nil || m.pane != msg.pane {
			return m, nil
		}
		m.pane.sending = false
		if msg.err != nil {
			// The composer keeps the text, so a refused message is corrected rather than retyped.
			m.setStatus("not sent: "+termsafe.Text(msg.err.Error()), true)
			return m, nil
		}
		m.setStatus("", false)
		m.pane.composer.Reset()
		m.pane.put(msg.msg, m.width)
		return m, nil
	case previewMsg:
		m.home.previewed(msg)
		if msg.err != nil {
			m.setStatus(termsafe.Text(msg.err.Error()), true)
		} else {
			m.setStatus("", false)
		}
		return m, nil
	case joinedMsg:
		if msg.err != nil {
			m.setStatus("could not join: "+termsafe.Text(msg.err.Error()), true)
			return m, nil
		}
		m.home.joined(msg.guild.Id)
		m.setStatus("joined "+termsafe.Text(msg.guild.Name), false)
		if m.sess == nil {
			return m, nil
		}
		return m, loadHome(m.gen, m.sess)

	case tea.KeyPressMsg:
		return m.onKey(msg)
	case tea.PasteMsg:
		return m.onPaste(msg)
	}
	return m, nil
}

func (m Model) quit() (tea.Model, tea.Cmd) {
	if m.sess != nil {
		_ = m.sess.Close()
		m.sess = nil
	}
	return m, tea.Quit
}

// onKey handles the keys that mean the same everywhere, then hands the rest to home or the pane.
//
// Every key is a docs/design/tui/KEYMAP.md convention, so "no chords beyond quit" holds: C-x C-c detaches
// (the daemon keeps running), RET is the primary action, ESC goes back, C-n/C-p move in a list. C-c alone
// does not quit, since M44 makes it a prefix; it says how to.
func (m Model) onKey(k tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := k.Keystroke()
	if m.armed {
		m.armed = false
		if key == "ctrl+c" {
			return m.quit()
		}
		m.setStatus("C-x "+chordName(key)+" is not bound; C-x C-c quits", true)
		return m, nil
	}
	switch key {
	case "ctrl+x":
		m.armed = true
		m.setStatus("C-x-", false)
		return m, nil
	case "ctrl+c":
		m.setStatus("C-x C-c to quit", false)
		return m, nil
	}

	if m.pane != nil {
		if key == "esc" {
			m.pane = nil
			m.setStatus("", false)
			return m, nil
		}
		return m.paneKey(k)
	}
	return m.homeKey(k)
}

func (m Model) onPaste(p tea.PasteMsg) (tea.Model, tea.Cmd) {
	if m.pane != nil {
		var cmd tea.Cmd
		m.pane.composer, cmd = m.pane.composer.Update(p)
		return m, cmd
	}
	var cmd tea.Cmd
	m.home.code, cmd = m.home.code.Update(p)
	m.home.codeChanged()
	return m, cmd
}

// chordName writes a key as KEYMAP.md does, C-x rather than ctrl+x.
func chordName(key string) string {
	return termsafe.Text(strings.ReplaceAll(strings.ReplaceAll(key, "ctrl+", "C-"), "alt+", "M-"))
}

// ---------- view ----------

// View draws the frame.
func (m Model) View() tea.View {
	v := tea.NewView(m.render())
	v.AltScreen = true
	return v
}

func (m Model) render() string {
	if m.width == 0 {
		return ""
	}
	if m.width < minWidth || m.height < minHeight {
		return clip(fmt.Sprintf("needs %d×%d; this is %d×%d", minWidth, minHeight, m.width, m.height), m.width)
	}
	var body string
	if m.pane != nil {
		body = m.pane.view(m.width, m.height-1)
	} else {
		body = m.home.view(m.account, m.width, m.height-1)
	}
	return body + "\n" + m.hintRow()
}

func (m Model) hintRow() string {
	if m.status != "" {
		if m.statusErr {
			return clip(sDanger.Render(m.status), m.width)
		}
		return clip(sWarn.Render(m.status), m.width)
	}
	if m.pane != nil {
		return clip(sDim.Render("RET send · ESC home · PgUp/PgDn scroll · C-x C-c quit"), m.width)
	}
	return clip(sDim.Render("RET open · C-n/C-p move · C-x C-c quit · norite about"), m.width)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
