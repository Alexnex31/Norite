// Package clierr holds the error values `main` decides an exit code from.
//
// One value lives here, and it is here rather than in the package that returns it because `main` is what
// gives it its meaning. A command that needs an answer and has no terminal to ask for one has a usage
// problem, not a crash: it exits 2 and prints without the "norite:" prefix. That mapping is a single
// `errors.Is` in cmd/app/main.go, and it can only stay complete if there is one thing to match.
//
// Three packages returned three separately-declared sentinels with this meaning before M10 — the wizard's,
// `norite login`'s, and `norite instance bootstrap`'s. `main` knew two of them, so bootstrap exited 1 with
// the prefix that makes a message read like an internal failure. Nothing was wrong with any of the three
// declarations; the fault was that adding a fourth command meant remembering to edit a file in a different
// directory, and the first command added after the rule was written did not.
package clierr

import "errors"

// ErrNoTerminal is returned when a command needs an answer and there is nowhere to ask for one.
//
// Wrap it rather than returning it bare, so each question can say what would have answered it: an email
// address and a password are missing for the same reason and are fixed by different flags. The exit code
// is the same either way, which is what lets a script tell "I did not supply an input" apart from "the
// credentials were wrong".
var ErrNoTerminal = errors.New("this command needs an interactive terminal to ask its questions")
