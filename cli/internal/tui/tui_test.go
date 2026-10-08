// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/apicontract"
	"github.com/Alexnex31/Norite/cli/internal/daemontest"
	"github.com/Alexnex31/Norite/daemon/ipc"
)

// The client is tested by driving its Update and View directly at a fixed size, against daemontest's fake
// daemon, which holds every request to openapi.yaml and fans a sent message out as MESSAGE_CREATE. Each
// command runs on its own goroutine and its message is fed back in, as Bubble Tea's own loop does, and a
// test waits for what the screen should show rather than for a number of steps. teatest is not used: it has
// never been tagged (M20a's plan, finding 10).

var at = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

func ptr[T any](v T) *T { return &v }

func guild(id, name string) apicontract.Guild {
	return apicontract.Guild{Id: id, Name: name, OwnerId: "1", CreatedAt: at, UpdatedAt: at}
}

func channel(id, guildID, name string) apicontract.Channel {
	return apicontract.Channel{Id: id, GuildId: ptr(guildID), Name: ptr(name), CreatedAt: at, UpdatedAt: at,
		PermissionOverwrites: []apicontract.PermissionOverwrite{}}
}

func message(id, channelID, authorID, author, content string) apicontract.Message {
	return apicontract.Message{Id: id, ChannelId: channelID, AuthorId: ptr(authorID), Content: content,
		CreatedAt: at, Author: &apicontract.PublicUser{Id: authorID, Username: strings.ToLower(author),
			DisplayName: author}}
}

func signedIn(name string) ipc.Ready {
	return ipc.Ready{Standing: ipc.StandingSignedIn, Guilds: []ipc.GuildSummary{},
		Account: &ipc.Account{InstanceURL: "https://chat.example", UserID: "1", Username: name}}
}

// fixture is an instance with one guild and its #general, and a fake daemon answering for it. sendMessage
// stores what it is sent, so a page read afterwards includes it, and the fake fans it out.
type fixture struct {
	d    *daemontest.Daemon
	mu   sync.Mutex
	held []apicontract.Message
	next atomic.Int64
}

func newFixture(t *testing.T) *fixture {
	f := &fixture{d: daemontest.New(t)}
	f.next.Store(100)
	f.d.On("listCurrentUserGuilds", daemontest.OK([]apicontract.Guild{guild("10", "Guild")})).
		On("listGuildChannels", daemontest.OK([]apicontract.Channel{channel("20", "10", "general")})).
		On("listChannelMessages", func(daemontest.Request) (int, any) {
			f.mu.Lock()
			defer f.mu.Unlock()
			page := []apicontract.Message{}
			for i := len(f.held) - 1; i >= 0; i-- {
				page = append(page, f.held[i])
			}
			return 200, page
		}).
		On("sendMessage", func(r daemontest.Request) (int, any) {
			var body struct{ Content string }
			_ = json.Unmarshal(r.Body, &body)
			m := message(fmt.Sprint(f.next.Add(1)), "20", "1", "Alice", body.Content)
			f.mu.Lock()
			f.held = append(f.held, m)
			f.mu.Unlock()
			return 201, m
		})
	return f
}

func (f *fixture) dialer(ready ipc.Ready, dials *atomic.Int64) Dialer {
	return func(context.Context) (Session, error) {
		if dials != nil {
			dials.Add(1)
		}
		return f.d.Attach(ready, true), nil
	}
}

// driver runs a Model as Bubble Tea would.
type driver struct {
	t    *testing.T
	m    tea.Model
	msgs chan tea.Msg
	quit bool
}

func drive(t *testing.T, opts Options, width, height int) *driver {
	t.Helper()
	d := &driver{t: t, m: New(opts), msgs: make(chan tea.Msg, 256)}
	d.send(tea.WindowSizeMsg{Width: width, Height: height})
	d.run(d.m.Init())
	return d
}

func (d *driver) run(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	go func() { d.msgs <- cmd() }()
}

func (d *driver) send(msg tea.Msg) {
	switch msg := msg.(type) {
	case nil:
		return
	case tea.BatchMsg:
		for _, c := range msg {
			d.run(c)
		}
		return
	case tea.QuitMsg:
		d.quit = true
		return
	}
	var cmd tea.Cmd
	d.m, cmd = d.m.Update(msg)
	d.run(cmd)
}

// until feeds messages in until the screen satisfies cond.
func (d *driver) until(what string, cond func(screen string) bool) {
	d.t.Helper()
	deadline := time.After(5 * time.Second)
	for !cond(d.screen()) {
		select {
		case msg := <-d.msgs:
			d.send(msg)
		case <-deadline:
			d.t.Fatalf("waiting for %s; the screen is:\n%s", what, d.screen())
		}
	}
}

func (d *driver) shows(what string) {
	d.t.Helper()
	d.until(fmt.Sprintf("%q on screen", what), func(s string) bool { return strings.Contains(s, what) })
}

// settle feeds in whatever is already waiting, so a test can assert on something that should not happen.
func (d *driver) settle() {
	for {
		select {
		case msg := <-d.msgs:
			d.send(msg)
		case <-time.After(100 * time.Millisecond):
			return
		}
	}
}

func (d *driver) raw() string    { return d.m.View().Content }
func (d *driver) screen() string { return ansi.Strip(d.raw()) }

func (d *driver) press(keys ...string) {
	for _, k := range keys {
		var msg tea.KeyPressMsg
		switch k {
		case "enter":
			msg = tea.KeyPressMsg{Code: tea.KeyEnter}
		case "esc":
			msg = tea.KeyPressMsg{Code: tea.KeyEscape}
		case "pgup":
			msg = tea.KeyPressMsg{Code: tea.KeyPgUp}
		case "pgdown":
			msg = tea.KeyPressMsg{Code: tea.KeyPgDown}
		default:
			ctrl, ok := strings.CutPrefix(k, "ctrl+")
			if !ok {
				d.t.Fatalf("press %q: type text with typeText", k)
			}
			msg = tea.KeyPressMsg{Code: rune(ctrl[0]), Mod: tea.ModCtrl}
		}
		d.send(msg)
	}
}

func (d *driver) typeText(s string) {
	for _, r := range s {
		d.send(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
}

func (m Model) attached() Session { return m.sess }

// ---------- the done-when ----------

// TestAMessageSentFromOnePaneReachesAnother is the milestone's done-when, two clients on one fake daemon:
// one opens home, opens #general from it and sends; the other, already in #general, draws the message as
// the daemon forwards it. The sender's own copy arrives twice — its answer and its MESSAGE_CREATE — and is
// drawn once, merged by id.
func TestAMessageSentFromOnePaneReachesAnother(t *testing.T) {
	f := newFixture(t)
	f.held = []apicontract.Message{message("50", "20", "2", "Bob", "earlier")}

	reader := drive(t, Options{Dial: f.dialer(signedIn("bob"), nil), Channel: "20"}, 80, 24)
	reader.shows("earlier")

	writer := drive(t, Options{Dial: f.dialer(signedIn("alice"), nil)}, 80, 24)
	writer.shows("# general")
	writer.shows("signed in as @alice")
	writer.press("enter")
	writer.shows("earlier")
	writer.typeText("hello from the pane")
	writer.press("enter")
	writer.shows("hello from the pane")

	reader.shows("hello from the pane")
	reader.shows("Alice")

	writer.settle()
	assert.Equal(t, 1, strings.Count(writer.screen(), "hello from the pane"), "sent once, drawn once")
	assert.NotContains(t, writer.screen(), "› hello from the pane", "the composer was cleared")
}

// TestASecondAccountJoinsFromHome: an account in no guild sees `5b`'s empty home, pastes a code, sees where
// it leads, joins with a second RET, and finds the guild's channel in its list.
func TestASecondAccountJoinsFromHome(t *testing.T) {
	d := daemontest.New(t)
	var joined atomic.Bool
	preview := apicontract.GuildInvitePreview{Code: "BCDFGHJKMNPQRSTV",
		Inviter: &apicontract.PublicUser{Id: "1", Username: "alice", DisplayName: "Alice"}}
	preview.Guild.Id, preview.Guild.Name = "10", "Guild"
	preview.Channel.Id, preview.Channel.Name = "20", ptr("general")
	d.On("listCurrentUserGuilds", func(daemontest.Request) (int, any) {
		if joined.Load() {
			return 200, []apicontract.Guild{guild("10", "Guild")}
		}
		return 200, []apicontract.Guild{}
	}).
		On("listGuildChannels", daemontest.OK([]apicontract.Channel{channel("20", "10", "general")})).
		On("previewInvite", daemontest.OK(preview)).
		On("redeemInvite", func(daemontest.Request) (int, any) {
			joined.Store(true)
			return 200, guild("10", "Guild")
		})

	c := drive(t, Options{Dial: func(context.Context) (Session, error) {
		return d.Attach(signedIn("bob"), true), nil
	}}, 80, 24)
	c.shows("NO GUILDS YET")
	c.typeText("bcdf-ghjk-mnpq-rstv")
	c.press("enter")
	c.shows("invited by Alice (@alice)")
	assert.False(t, joined.Load(), "one RET only looks; it does not join")
	c.press("enter")
	c.shows("joined Guild")
	c.shows("# general")
	assert.NotContains(t, c.screen(), "NO GUILDS YET")

	var redeemed []daemontest.Request
	for _, r := range d.Requests() {
		if r.Op == "redeemInvite" {
			redeemed = append(redeemed, r)
		}
	}
	require.Len(t, redeemed, 1)
	assert.JSONEq(t, `{"code":"bcdf-ghjk-mnpq-rstv"}`, string(redeemed[0].Body))
}

// TestAStrangersTextIsDrawnInert: a message carrying ESC[2J and an OSC title sequence, by an author whose
// name carries a bidi override, draws as text. Asserted on the frame as drawn, before anything strips it:
// the frame's own styling is SGR, and neither of these is.
func TestAStrangersTextIsDrawnInert(t *testing.T) {
	f := newFixture(t)
	hostile := message("50", "20", "2", "Mallory\u202eyrollam", "clear\x1b[2Jthe screen\x1b]0;owned\x07 \u202edone")
	f.held = []apicontract.Message{hostile}

	c := drive(t, Options{Dial: f.dialer(signedIn("bob"), nil), Channel: "20"}, 80, 24)
	c.shows("the screen")
	for _, bad := range []string{"\x1b[2J", "\x1b]0;", "\x07", "\u202e"} {
		assert.NotContains(t, c.raw(), bad)
	}
}

// TestQuittingClosesTheSocket: C-x C-c detaches — the daemon keeps running — and ends the program.
func TestQuittingClosesTheSocket(t *testing.T) {
	f := newFixture(t)
	c := drive(t, Options{Dial: f.dialer(signedIn("bob"), nil)}, 80, 24)
	c.shows("# general")
	s := c.m.(Model).attached()
	require.NotNil(t, s)

	c.press("ctrl+x", "ctrl+c")
	c.settle()
	assert.True(t, c.quit)
	select {
	case <-s.Done():
	default:
		t.Fatal("the attachment is still open after quitting")
	}
}

// TestCtrlCAloneSaysHowToQuit: C-c is M44's prefix, so on its own it explains rather than quits.
func TestCtrlCAloneSaysHowToQuit(t *testing.T) {
	f := newFixture(t)
	c := drive(t, Options{Dial: f.dialer(signedIn("bob"), nil)}, 80, 24)
	c.shows("# general")
	c.press("ctrl+c")
	c.settle()
	assert.False(t, c.quit)
	c.shows("C-x C-c to quit")
}

// ---------- when the ground moves ----------

// TestAResyncReattachesAndRereads: the daemon closing the client with resync means the state it was built
// from was cleared, so the client attaches again and reads the open channel afresh.
func TestAResyncReattachesAndRereads(t *testing.T) {
	f := newFixture(t)
	var dials atomic.Int64
	c := drive(t, Options{Dial: f.dialer(signedIn("bob"), &dials), Channel: "20"}, 80, 24)
	c.shows("No messages yet")

	f.mu.Lock()
	f.held = []apicontract.Message{message("60", "20", "2", "Bob", "said while away")}
	f.mu.Unlock()
	dropped := time.Now()
	f.d.Drop(&ipc.CloseError{Code: ipc.CloseResync, Reason: "resync"})
	c.shows("said while away")
	assert.EqualValues(t, 2, dials.Load())
	// At once, not after the retry a stopped daemon gets: a resync is the daemon asking to be read again.
	assert.Less(t, time.Since(dropped), retryFirst/2)
}

// TestAStoppedDaemonIsWaitedFor: an attach that fails says so in the hint row and is tried again; quitting
// still works while it waits.
func TestAStoppedDaemonIsWaitedFor(t *testing.T) {
	c := drive(t, Options{Dial: func(context.Context) (Session, error) {
		return nil, errors.New("the daemon is not running")
	}}, 80, 24)
	c.shows("trying again in 1s · the daemon is not running")
	c.press("ctrl+x", "ctrl+c")
	c.settle()
	assert.True(t, c.quit)
}

// TestASignedOutDaemonSaysToLogIn: the client keeps waiting, for the resync a login brings.
func TestASignedOutDaemonSaysToLogIn(t *testing.T) {
	f := newFixture(t)
	c := drive(t, Options{Dial: f.dialer(ipc.Ready{Standing: ipc.StandingSignedOut, Guilds: []ipc.GuildSummary{}}, nil)},
		80, 24)
	c.shows("signed out; run `norite login`")
}

// TestADeletedChannelStopsTheComposer: the pane says what happened, refuses to send, and ESC goes home.
func TestADeletedChannelStopsTheComposer(t *testing.T) {
	f := newFixture(t)
	c := drive(t, Options{Dial: f.dialer(signedIn("bob"), nil), Channel: "20"}, 80, 24)
	c.shows("No messages yet")
	// Typed before the deletion, so it is the guard that stops it and not the composer being empty.
	c.typeText("written before the delete")
	f.d.Dispatch("CHANNEL_DELETE", json.RawMessage(`{"id":"20","guild_id":"10"}`))
	c.shows("this channel was deleted")
	c.press("enter")
	c.settle()
	for _, r := range f.d.Requests() {
		assert.NotEqual(t, "sendMessage", r.Op, "nothing is sent into a deleted channel")
	}
	c.press("esc")
	c.shows("YOUR GUILDS")
}

// TestARefusedSendKeepsTheText: a mute's 403 says why, and the composer keeps what was typed.
func TestARefusedSendKeepsTheText(t *testing.T) {
	f := newFixture(t)
	f.d.On("sendMessage", func(daemontest.Request) (int, any) {
		return 403, map[string]any{"error": map[string]any{"code": "forbidden",
			"message": "you may not send messages here", "request_id": "r1"}}
	})
	c := drive(t, Options{Dial: f.dialer(signedIn("bob"), nil), Channel: "20"}, 80, 24)
	c.shows("No messages yet")
	c.typeText("let me speak")
	c.press("enter")
	c.shows("not sent")
	c.shows("you may not send messages here")
	assert.Contains(t, c.screen(), "let me speak", "the composer keeps the text")
}

// TestAnEmptyMessageIsRefusedHere: nothing is sent for an empty composer.
func TestAnEmptyMessageIsRefusedHere(t *testing.T) {
	f := newFixture(t)
	c := drive(t, Options{Dial: f.dialer(signedIn("bob"), nil), Channel: "20"}, 80, 24)
	c.shows("No messages yet")
	c.press("enter")
	c.shows("the message is empty")
	for _, r := range f.d.Requests() {
		assert.NotEqual(t, "sendMessage", r.Op)
	}
}

// TestAMessageArrivingDuringTheReadIsKept: the client attaches for events before it reads the page, and
// merges the two by id, so a message that arrives while the page is read is neither lost nor drawn twice.
func TestAMessageArrivingDuringTheReadIsKept(t *testing.T) {
	f := newFixture(t)
	during := message("71", "20", "2", "Bob", "arrived during the read")
	f.d.On("listChannelMessages", func(daemontest.Request) (int, any) {
		raw, _ := json.Marshal(during)
		f.d.Dispatch("MESSAGE_CREATE", raw)
		time.Sleep(50 * time.Millisecond) // the event is applied first
		// The page was read before the message was sent, so it does not carry it: replacing what the pane
		// holds with the page would lose the message, and merging keeps it.
		return 200, []apicontract.Message{message("70", "20", "2", "Bob", "before it")}
	})
	c := drive(t, Options{Dial: f.dialer(signedIn("bob"), nil), Channel: "20"}, 80, 24)
	c.shows("before it")
	c.shows("arrived during the read")
	c.settle()
	assert.Equal(t, 1, strings.Count(c.screen(), "arrived during the read"))
	assert.Less(t, strings.Index(c.screen(), "before it"), strings.Index(c.screen(), "arrived during the read"),
		"in id order")
}

// TestATooSmallTerminalGetsOneLine: README's grid draws a single line below 40×12.
func TestATooSmallTerminalGetsOneLine(t *testing.T) {
	f := newFixture(t)
	c := drive(t, Options{Dial: f.dialer(signedIn("bob"), nil)}, 39, 30)
	c.settle()
	assert.Equal(t, "needs 40×12; this is 39×30", c.screen(), "one line, whole, and nothing else")
}

// ---------- the pane's own bounds ----------

func TestAPaneHoldsAtMostFiveHundred(t *testing.T) {
	p := newPane("10", "20")
	for i := range 600 {
		p.put(message(fmt.Sprint(1000+i), "20", "2", "Bob", "x"), 80)
	}
	require.Len(t, p.msgs, maxHeld)
	assert.Equal(t, "1100", p.msgs[0].Id, "the oldest are dropped")
	assert.Equal(t, "1599", p.msgs[len(p.msgs)-1].Id)
}

// TestAMessageWrittenByAutomationIsDrawnTagged is the second half of M22's done-when: what a token wrote
// says so beside its author, and what a person typed does not.
func TestAMessageWrittenByAutomationIsDrawnTagged(t *testing.T) {
	typed := message("1", "20", "2", "Bob", "by hand")
	scripted := message("2", "20", "2", "Bob", "by a script")
	scripted.Type = 1

	header := func(m apicontract.Message, width int) string {
		return ansi.Strip(defaultLook.messageLines(m, width)[0])
	}
	assert.NotContains(t, header(typed, 80), "AUTO")
	assert.Regexp(t, `^Bob AUTO  `, header(scripted, 80))
	assert.Contains(t, ansi.Strip(strings.Join(defaultLook.messageLines(scripted, 80), "\n")), "by a script")

	// A name as long as an instance allows, in characters two cells wide, in the narrowest pane there is.
	// The name is what is cut.
	scripted.Author.DisplayName = strings.Repeat("名", 64)
	for _, width := range []int{40, 20, 12} {
		got := header(scripted, width)
		assert.Contains(t, got, "AUTO", "at %d columns the tag was cut off: %q", width, got)
		assert.LessOrEqual(t, ansi.StringWidth(got), width)
	}
	// And a person whose name is long is drawn as before, with no room taken for a tag.
	typed.Author.DisplayName = strings.Repeat("名", 64)
	assert.NotContains(t, header(typed, 40), "AUTO")
}

func TestWhatThePaneCannotNameItSaysSo(t *testing.T) {
	deleted := message("1", "20", "2", "Bob", "hi")
	deleted.Author = nil
	system := message("2", "20", "2", "Bob", "hi")
	system.Author, system.AuthorId = nil, nil
	strange := message("3", "20", "2", "Bob", "hi")
	strange.Type = 9

	assert.Contains(t, ansi.Strip(strings.Join(defaultLook.messageLines(deleted, 80), "\n")), "deleted account")
	assert.Contains(t, ansi.Strip(strings.Join(defaultLook.messageLines(system, 80), "\n")), "system")
	assert.Contains(t, ansi.Strip(strings.Join(defaultLook.messageLines(strange, 80), "\n")), "cannot show")
}

// TestScrollingUpStaysPut: PgUp leaves the bottom, and a message arriving then does not move the view; only
// at the bottom does the pane follow.
func TestScrollingUpStaysPut(t *testing.T) {
	f := newFixture(t)
	for i := range 40 {
		f.held = append(f.held, message(fmt.Sprint(200+i), "20", "2", "Bob", fmt.Sprintf("line %02d", i)))
	}
	c := drive(t, Options{Dial: f.dialer(signedIn("bob"), nil), Channel: "20"}, 80, 24)
	c.shows("line 39")
	c.press("pgup")
	before := c.screen()
	assert.NotContains(t, before, "line 39")
	raw, _ := json.Marshal(message("300", "20", "2", "Bob", "newest"))
	f.d.Dispatch("MESSAGE_CREATE", raw)
	c.settle()
	assert.Equal(t, before, c.screen(), "scrolled up, the view does not move")
	c.press("pgdown", "pgdown", "pgdown")
	c.shows("newest")
}

// ---------- what the manual pass found (M20a, part 10) ----------

// signedInAs is signedIn for a named account: sameAccount tells sign-ins apart by user id.
func signedInAs(name, userID string) ipc.Ready {
	r := signedIn(name)
	r.Account.UserID = userID
	return r
}

// switchable dials whatever READY the test last set, so a resync can land on a different sign-in.
type switchable struct {
	f     *fixture
	mu    sync.Mutex
	ready ipc.Ready
}

func (s *switchable) set(r ipc.Ready) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ready = r
}

func (s *switchable) dial(context.Context) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.f.d.Attach(s.ready, true), nil
}

// TestASignOutForgetsWhatWasDrawn: after a logout, the conversation the ended sign-in was shown is gone from
// the screen, as it is from the daemon, and home says how to get it back.
func TestASignOutForgetsWhatWasDrawn(t *testing.T) {
	f := newFixture(t)
	f.held = []apicontract.Message{message("60", "20", "2", "Bob", "only bob may read this")}
	s := &switchable{f: f, ready: signedInAs("bob", "2")}
	c := drive(t, Options{Dial: s.dial, Channel: "20"}, 80, 24)
	c.shows("only bob may read this")

	s.set(ipc.Ready{Standing: ipc.StandingSignedOut, Guilds: []ipc.GuildSummary{}})
	f.d.Drop(&ipc.CloseError{Code: ipc.CloseResync, Reason: "the daemon's sign-in ended"})
	c.shows("signed out; run `norite login`")
	assert.NotContains(t, c.screen(), "only bob may read this")
	c.shows("Sign in with `norite login` to see your guilds")
}

// TestAnotherAccountDoesNotInheritTheScreen: a resync landing on a different account drops the open pane,
// draft included, rather than drawing one account's conversation under the other's name.
func TestAnotherAccountDoesNotInheritTheScreen(t *testing.T) {
	f := newFixture(t)
	s := &switchable{f: f, ready: signedInAs("bob", "2")}
	c := drive(t, Options{Dial: s.dial, Channel: "20"}, 80, 24)
	c.shows("No messages yet")
	c.typeText("typed as bob")

	s.set(signedInAs("alice", "3"))
	f.d.Drop(&ipc.CloseError{Code: ipc.CloseResync, Reason: "the daemon's sign-in ended"})
	c.shows("signed in as @alice")
	c.settle()
	assert.Nil(t, c.m.(Model).pane, "the pane opened as bob is closed")
	assert.NotContains(t, c.screen(), "typed as bob")
}

// TestAResyncDropsWhatWasDeletedInTheGap: a resync reads the open channel afresh rather than merging into it,
// so a message deleted while the client was detached does not stay on screen.
func TestAResyncDropsWhatWasDeletedInTheGap(t *testing.T) {
	f := newFixture(t)
	f.held = []apicontract.Message{message("60", "20", "2", "Bob", "deleted while away")}
	c := drive(t, Options{Dial: f.dialer(signedIn("bob"), nil), Channel: "20"}, 80, 24)
	c.shows("deleted while away")

	f.mu.Lock()
	f.held = nil
	f.mu.Unlock()
	f.d.Drop(&ipc.CloseError{Code: ipc.CloseResync, Reason: "resync"})
	c.shows("No messages yet")
	assert.NotContains(t, c.screen(), "deleted while away")
}

// TestAGoneChannelClearsAnOldRefusal: once the channel is gone, a refused send's error no longer describes
// anything, and the status row stops showing it.
func TestAGoneChannelClearsAnOldRefusal(t *testing.T) {
	f := newFixture(t)
	f.d.On("sendMessage", func(daemontest.Request) (int, any) {
		return 403, map[string]any{"error": map[string]any{"code": "forbidden", "message": "forbidden",
			"request_id": "r1"}}
	})
	c := drive(t, Options{Dial: f.dialer(signedIn("bob"), nil), Channel: "20"}, 80, 24)
	c.shows("No messages yet")
	c.typeText("let me speak")
	c.press("enter")
	c.shows("not sent")
	f.d.Dispatch("CHANNEL_DELETE", json.RawMessage(`{"id":"20","guild_id":"10"}`))
	c.shows("this channel was deleted")
	assert.NotContains(t, c.screen(), "not sent")
}

// TestAGuildWithNoTextChannelIsStillListed: a guild whose text channels are all hidden or deleted is one the
// account is still in, so home lists it rather than saying there are no guilds.
func TestAGuildWithNoTextChannelIsStillListed(t *testing.T) {
	f := newFixture(t)
	f.d.On("listGuildChannels", daemontest.OK([]apicontract.Channel{}))
	c := drive(t, Options{Dial: f.dialer(signedIn("bob"), nil)}, 80, 24)
	c.shows("no text channel you can see")
	assert.Contains(t, c.screen(), "Guild")
	assert.NotContains(t, c.screen(), "NO GUILDS YET")
}

// TestANarrowTerminalStillSaysWhenItRetries: the retry leads the status row, so the width cuts the dial
// error's socket path rather than when the client will try again.
func TestANarrowTerminalStillSaysWhenItRetries(t *testing.T) {
	c := drive(t, Options{Dial: func(context.Context) (Session, error) {
		return nil, errors.New("the daemon is not running: nothing is listening at /tmp/a/rather/long/state/dir/norite/daemon.sock")
	}}, 40, 12)
	c.shows("trying again in 1s")
}

// TestAChannelWhoseHistoryCannotBeReadSaysSo: a member who may view and post but not read the backlog (M15's
// support-thread configuration) is told the earlier messages could not be read, not that there are none.
func TestAChannelWhoseHistoryCannotBeReadSaysSo(t *testing.T) {
	f := newFixture(t)
	f.d.On("listChannelMessages", func(daemontest.Request) (int, any) {
		return 403, map[string]any{"error": map[string]any{"code": "forbidden", "message": "forbidden",
			"request_id": "r1"}}
	})
	c := drive(t, Options{Dial: f.dialer(signedIn("bob"), nil), Channel: "20"}, 100, 24)
	c.shows("earlier messages could not be read")
	assert.NotContains(t, c.screen(), "No messages yet")

	// And what arrives live is still drawn.
	raw, _ := json.Marshal(message("300", "20", "2", "Bob", "arrived live"))
	f.d.Dispatch("MESSAGE_CREATE", raw)
	c.shows("arrived live")
}

// TestALateSendAnswerLeavesAReopenedPaneAlone: a send answered after its pane was closed and the channel
// reopened belongs to the closed pane, and does not clear what is being typed in the new one.
func TestALateSendAnswerLeavesAReopenedPaneAlone(t *testing.T) {
	f := newFixture(t)
	release := make(chan struct{})
	f.d.On("sendMessage", func(r daemontest.Request) (int, any) {
		<-release
		return 201, message("400", "20", "1", "Alice", "first")
	})
	c := drive(t, Options{Dial: f.dialer(signedIn("alice"), nil)}, 80, 24)
	c.shows("# general")
	c.press("enter")
	c.shows("No messages yet")
	c.typeText("first")
	c.press("enter")
	c.press("esc")
	c.shows("YOUR GUILDS")
	c.press("enter")
	c.shows("No messages yet")
	c.typeText("the new draft")
	close(release)
	c.settle()
	assert.Contains(t, c.screen(), "the new draft", "the reopened pane keeps its draft")
}
