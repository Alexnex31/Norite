// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package prompt asks the one question a command asks before it does something somebody may not have
// meant: yes or no.
//
// It is one implementation because the answer is a contract a script relies on (M20): --yes answers it;
// on a terminal anything but y or yes declines, which is a usage error, since the command did nothing
// because it was told not to; and with no terminal and no --yes it is clierr.ErrNoTerminal naming the
// flag, so a script that forgot it exits 2 rather than hanging on a question nobody sees, and never
// proceeds by default. The verbs and `norite config import` each had a copy of it, in packages that
// cannot see each other, and a change to one would have left the other behind (M21 /code-review).
package prompt

import (
	"bufio"
	"fmt"
	"io"
	"strings"

	"github.com/Alexnex31/Norite/cli/internal/clierr"
)

// Confirm is one question.
type Confirm struct {
	// Yes is the --yes flag: the question is answered, and not asked.
	Yes bool
	// Interactive reports that In is a terminal somebody is at.
	Interactive bool
	In          io.Reader
	// Out is where the question is written, which is stderr: stdout is the result, which --json pipes
	// into a parser, and a question written there would both vanish into the pipe and corrupt the
	// document.
	Out io.Writer
	// Question is asked as written, followed by " [y/N] ". The caller has made it safe to print.
	Question string
	// Otherwise completes "…: " when there is no terminal: what to pass instead of answering.
	Otherwise string
}

// Asks reports whether Ask will put the question to somebody, for a caller that has something to show
// them first.
func (c Confirm) Asks() bool { return !c.Yes && c.Interactive }

// Ask returns nil when the answer is yes.
func (c Confirm) Ask() error {
	if c.Yes {
		return nil
	}
	if !c.Interactive {
		return fmt.Errorf("%w: %s", clierr.ErrNoTerminal, c.Otherwise)
	}
	if _, err := fmt.Fprintf(c.Out, "%s [y/N] ", c.Question); err != nil {
		return err
	}
	line, _ := bufio.NewReader(c.In).ReadString('\n')
	if Yes(line, false) {
		return nil
	}
	return clierr.Usage("not confirmed; nothing was changed")
}

// Yes reports whether a typed answer is a yes: y or yes, in any case, with space around it ignored. An
// empty answer is byDefault, and anything else is no.
//
// It is the one definition of an answer, for Confirm and for a question that is not a Confirm: the offer
// `norite login` ends with declines without an error and may default to yes, which Confirm does neither
// of, and it must still mean the same thing by "yes" (M23 /code-review).
func Yes(answer string, byDefault bool) bool {
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "":
		return byDefault
	case "y", "yes":
		return true
	}
	return false
}
