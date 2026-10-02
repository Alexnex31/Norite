// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"errors"
	"fmt"
	"testing"

	"github.com/urfave/cli/v3"

	"github.com/Alexnex31/Norite/cli/internal/clierr"
	"github.com/Alexnex31/Norite/cli/internal/instanceadmin"
	"github.com/Alexnex31/Norite/cli/internal/instanceinit"
	"github.com/Alexnex31/Norite/cli/internal/login"
)

// Every command that can run out of answers must report it the same way.
//
// This is the test that was missing at M10. Three packages declared their own "needs a terminal" sentinel
// and main matched two of them by name, so `norite instance bootstrap` exited 1 with the "norite:" prefix
// that makes a usage problem read like an internal failure — the exact outcome the branch exists to
// prevent. Pointing them all at one value is the fix; this pins that they still do, and a fourth package
// that declares its own instead of reusing clierr's fails here rather than in somebody's terminal.
func TestEveryTerminalSentinelIsTheSharedOne(t *testing.T) {
	for name, err := range map[string]error{
		"instanceinit.ErrNotATerminal":  instanceinit.ErrNotATerminal,
		"login.ErrNoTerminal":           login.ErrNoTerminal,
		"instanceadmin.ErrNoTerminal":   instanceadmin.ErrNoTerminal,
		"a wrapped one, as commands do": fmt.Errorf("%w: pass --username", instanceadmin.ErrNoTerminal),
	} {
		if !errors.Is(err, clierr.ErrNoTerminal) {
			t.Errorf("%s does not wrap clierr.ErrNoTerminal, so main will report it as a crash", name)
		}
	}
}

func TestReport(t *testing.T) {
	tests := []struct {
		name        string
		err         error
		wantCode    int
		wantMessage string
	}{
		{
			// Exit 2 and no prefix: a usage problem, whichever command hit it.
			name:     "a command with no terminal to ask in",
			err:      fmt.Errorf("%w: set NORITE_ADMIN_PASSWORD", instanceadmin.ErrNoTerminal),
			wantCode: 2,
			wantMessage: "this command needs an interactive terminal to ask its questions: " +
				"set NORITE_ADMIN_PASSWORD",
		},
		{
			name:        "an ordinary failure",
			err:         errors.New("something went wrong"),
			wantCode:    1,
			wantMessage: "norite: something went wrong",
		},
		{
			// Exit 2 is a usage error however it was raised, so it prints like ErrNoTerminal's: without the
			// prefix (M20). The prefix it does not carry is still never the command's to add — see
			// TestUsageMessagesDoNotCarryThePrefixMainAdds — or the exit-1 path would print it twice.
			name:        "a usage error from a command",
			err:         cli.Exit("which invite? Pass the code to revoke.", 2),
			wantCode:    2,
			wantMessage: "which invite? Pass the code to revoke.",
		},
		{
			name:        "a usage error found by a verb",
			err:         clierr.Usage("pass the guild's id"),
			wantCode:    2,
			wantMessage: "pass the guild's id",
		},
		{
			// The done-when's non-member: exit 4, distinct from a crash and from the caller's own mistake,
			// and printed as the instance's answer rather than as a failure of this program.
			name: "an instance refusing the request",
			err: fmt.Errorf("renaming the guild: %w", &clierr.RefusedError{
				Status: 404, Code: "not_found", Message: "not found", RequestID: "req-1",
			}),
			wantCode:    4,
			wantMessage: "renaming the guild: not found (request req-1)",
		},
		{
			name:        "a daemon that is not running",
			err:         clierr.Unavailable("the daemon is not running; start it with `norite daemon start`"),
			wantCode:    3,
			wantMessage: "norite: the daemon is not running; start it with `norite daemon start`",
		},
		{
			// `norite daemon status` reports through the code alone and has already said its piece.
			name:        "an exit code that is the whole message",
			err:         cli.Exit("", 1),
			wantCode:    1,
			wantMessage: "",
		},
		{
			// CLAUDE.md rule 19's backstop: the byte that acts on a terminal never reaches it. What
			// survives is the literal text "[2J", which is inert — the sanitizer removes what acts on a
			// terminal, not everything that looks like part of an escape sequence.
			name:        "an error carrying a terminal escape",
			err:         errors.New("server said \x1b[2Jgone"),
			wantCode:    1,
			wantMessage: "norite: server said �[2Jgone",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, message := report(tt.err)
			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d", code, tt.wantCode)
			}
			if message != tt.wantMessage {
				t.Errorf("message  = %q\nwant       %q", message, tt.wantMessage)
			}
		})
	}
}
