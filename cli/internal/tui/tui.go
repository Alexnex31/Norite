// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package tui is the terminal client (M20a): the smallest thing two people can hold a conversation in.
//
// Bare `norite` on a terminal opens home — who you are, your guilds with their text channels, and a box to
// redeem an invite — and a channel opens from it into one pane with a message list, a composer and quit.
// It draws reduced forms of two screens docs/design/tui/SCREENS.md specifies, `5b`'s redeem box over `1a`'s
// channel column folded into a list, and `1a`'s message area and composer; M41–M43 replace it with the real
// frame, rail and renderer. No panes or splits, no chords beyond quit, no theming, no scrollback search.
//
// # Where it reaches the instance
//
// Through the daemon, as an attach client asking for events, and through the same functions the verbs use
// (package ops): the token never crosses the socket, and a request this client sends is shaped and bounded
// exactly as `norite message send` would shape it.
//
// # What it draws, and how
//
// Plain text. Every name and every message passes termsafe before it is drawn (rule 19), and no markup is
// interpreted, which is how rule 9 is met before M43's renderer exists. Colors are TOKENS.md's roles on the
// terminal's own ANSI 0–15, the default its theme model names, with no setting to change them yet.
package tui

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"

	tea "charm.land/bubbletea/v2"

	"github.com/Alexnex31/Norite/cli/internal/daemonclient"
	"github.com/Alexnex31/Norite/daemon/ipc"
)

// Session is one attachment to the daemon with its event stream: what *ipc.Client is, as far as this client
// uses it, so the tests can attach to daemontest's fake instead.
type Session interface {
	daemonclient.Caller
	Ready() ipc.Ready
	Events() <-chan ipc.Event
	Done() <-chan struct{}
	Err() error
	Close() error
}

// Dialer attaches a new Session.
type Dialer func(ctx context.Context) (Session, error)

// DaemonDialer attaches to this user's daemon as a client of the given version, asking for its events.
func DaemonDialer(version string) Dialer {
	return func(ctx context.Context) (Session, error) {
		c, err := daemonclient.Watch(ctx, version)
		if err != nil {
			return nil, err
		}
		return c, nil
	}
}

// Options configure the client.
type Options struct {
	// Dial attaches to the daemon, and is called again whenever the attachment ends.
	Dial Dialer
	// Channel, when set, opens that channel directly rather than home. ESC still leads home.
	Channel string
}

// Run draws the client until it is quit, on the terminal's alternate screen.
//
// SIGTERM is Bubble Tea's to handle and SIGHUP is handled here: both restore the terminal and return nil, so
// the process exits 0, because neither is a failure of the client. Nothing is printed to the terminal while
// the client draws on it — no log line, no stray write — or it would corrupt the frame (rule 8's neighbor:
// nothing reaches a terminal the client did not mean to draw).
func Run(ctx context.Context, opts Options) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	p := tea.NewProgram(New(opts), tea.WithContext(ctx))

	hangup := make(chan os.Signal, 1)
	signal.Notify(hangup, syscall.SIGHUP)
	defer signal.Stop(hangup)
	go func() {
		select {
		case <-hangup:
			p.Quit()
		case <-ctx.Done():
		}
	}()

	final, err := p.Run()
	if m, ok := final.(Model); ok && m.sess != nil {
		_ = m.sess.Close()
	}
	if errors.Is(err, tea.ErrProgramKilled) && ctx.Err() != nil {
		return nil
	}
	return err
}
