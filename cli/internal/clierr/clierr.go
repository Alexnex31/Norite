// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package clierr holds the error values `main` decides an exit code from.
//
// The codes (architecture.md §4, M20):
//
//   - 0, success;
//   - 1, anything else;
//   - 2, a local usage error, printed without the "norite: " prefix: ErrNoTerminal, Usage, or a command's
//     own cli.Exit with code 2;
//   - 3, the command cannot be carried out here: Unavailable — no daemon running, not signed in, the
//     instance unreachable;
//   - 4, the instance refused the request: Refused, also without the prefix, so a script can tell its own
//     mistake from the instance's answer.
//
// The values live here rather than in the packages that return them because `main` is what gives them their
// meaning. A command that needs an answer and has no terminal to ask for one has a usage
// problem, not a crash: it exits 2 and prints without the "norite:" prefix. That mapping is a single
// `errors.Is` in cmd/app/main.go, and it can only stay complete if there is one thing to match.
//
// Three packages returned three separately-declared sentinels with this meaning before M10 — the wizard's,
// `norite login`'s, and `norite instance bootstrap`'s. `main` knew two of them, so bootstrap exited 1 with
// the prefix that makes a message read like an internal failure. Nothing was wrong with any of the three
// declarations; the fault was that adding a fourth command meant remembering to edit a file in a different
// directory, and the first command added after the rule was written did not.
package clierr

import (
	"errors"
	"fmt"
)

// ErrNoTerminal is returned when a command needs an answer and there is nowhere to ask for one.
//
// Wrap it rather than returning it bare, so each question can say what would have answered it: an email
// address and a password are missing for the same reason and are fixed by different flags. The exit code
// is the same either way, which is what lets a script tell "I did not supply an input" apart from "the
// credentials were wrong".
var ErrNoTerminal = errors.New("this command needs an interactive terminal to ask its questions")

// Exit codes, as main uses them.
const (
	ExitFailure     = 1
	ExitUsage       = 2
	ExitUnavailable = 3
	ExitRefused     = 4
)

// UsageError is a command used wrongly, found before anything was asked of anybody: a missing argument, two
// flags that contradict each other, a confirmation declined. Exit 2, no prefix.
type UsageError struct{ msg string }

func (e *UsageError) Error() string { return e.msg }

// Usage builds a UsageError.
func Usage(format string, args ...any) error {
	return &UsageError{msg: fmt.Sprintf(format, args...)}
}

// UnavailableError is a command that cannot be carried out from here, now: no daemon is running, the daemon
// is not signed in, the instance cannot be reached. Exit 3. Distinct from a failure because the remedy is
// somewhere else — start the daemon, sign in, wait — and a script retrying it should know that.
type UnavailableError struct{ msg string }

func (e *UnavailableError) Error() string { return e.msg }

// Unavailable builds an UnavailableError.
func Unavailable(format string, args ...any) error {
	return &UnavailableError{msg: fmt.Sprintf(format, args...)}
}

// RefusedError is the instance answering a request with a 4xx: not a member, not permitted, no such object,
// a value it would not accept. Exit 4, no prefix, because the program did what was asked of it and the
// answer was no — a non-member's refusal is a usage error in the done-when's sense, and must never read as
// a crash.
type RefusedError struct {
	Status int
	// Code is the instance's error code, sanitized: not_found, forbidden, validation_failed, …
	Code string
	// Message is the instance's own message, sanitized.
	Message string
	// RequestID is what an instance's operator needs to find the request in their log.
	RequestID string
}

func (e *RefusedError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = fmt.Sprintf("the instance refused the request (HTTP %d)", e.Status)
	}
	if e.RequestID != "" {
		return fmt.Sprintf("%s (request %s)", msg, e.RequestID)
	}
	return msg
}
