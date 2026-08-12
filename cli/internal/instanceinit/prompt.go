package instanceinit

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/term"
)

// ErrNotATerminal is returned when the wizard needs to ask a question but has nowhere to ask it.
//
// This is the failure mode the design specifically calls for (docs/architecture.md §4): the wizard has to
// work over SSH, inside `docker exec`, and in CI, and when stdin is not a terminal it must fail with an
// actionable message rather than hang forever waiting for input nobody is there to type — or, worse,
// read EOF and quietly accept every default including an empty database password.
var ErrNotATerminal = errors.New(
	"this command needs an interactive terminal to ask its questions; pass --non-interactive with the " +
		"flags for the values you want, or run it from a terminal")

// promptMode decides what happens when the wizard has a question to ask.
type promptMode int

const (
	// promptInteractive asks, and waits for an answer.
	promptInteractive promptMode = iota
	// promptScripted answers from flags and defaults without asking. This is --non-interactive: the
	// operator has stated that nothing should be asked, so a default is a deliberate choice, not a
	// guess made on their behalf.
	promptScripted
	// promptNoTerminal means a question needs asking but there is no terminal to ask it on, and the
	// operator did not say --non-interactive.
	//
	// This must be an error, not a silent fallback to defaults. Treating it as scripted is how a piped
	// or containerized run ends up writing a config with an empty database password and reporting
	// success — the operator never asked for defaults, they just weren't at a keyboard.
	promptNoTerminal
)

// prompter asks the operator questions, one at a time, over plain stdin/stdout.
//
// Deliberately not a Bubble Tea program. A full-screen UI would be more pleasant in a local terminal and
// worse everywhere else this actually runs, and it is a large amount of code and test surface for a flow
// most operators run exactly once.
type prompter struct {
	in   *bufio.Reader
	out  io.Writer
	mode promptMode

	// readPassword reads a secret without echoing it. Indirected so tests can drive it; in production it
	// is the terminal's own no-echo read.
	readPassword func() (string, error)
}

// newPrompter builds a prompter over an arbitrary reader.
func newPrompter(in io.Reader, out io.Writer, mode promptMode, readSecret func() (string, error)) *prompter {
	return &prompter{
		in:           bufio.NewReader(in),
		out:          out,
		mode:         mode,
		readPassword: readSecret,
	}
}

// asks reports whether questions actually reach a human. Output that only makes sense alongside a
// question — section headings, explanatory notes — is suppressed when they don't.
func (p *prompter) asks() bool { return p.mode == promptInteractive }

// unattended returns the error, if any, that stands in for asking a question.
func (p *prompter) unattended() error {
	if p.mode == promptNoTerminal {
		return ErrNotATerminal
	}
	return nil
}

// newTerminalPrompter builds a prompter over the real terminal.
func newTerminalPrompter(in *os.File, out io.Writer, allowInteractive bool) *prompter {
	fd := int(in.Fd())

	mode := promptInteractive
	switch {
	case !allowInteractive:
		mode = promptScripted
	case !term.IsTerminal(fd):
		mode = promptNoTerminal
	}

	return newPrompter(in, out, mode, func() (string, error) {
		b, err := term.ReadPassword(fd)
		if err != nil {
			return "", err
		}
		return string(b), nil
	})
}

// printf and println write to the prompt stream.
//
// A failed write to the terminal is not actionable — there is nowhere left to report it, and the wizard
// still has useful work to do — so the error is dropped deliberately, once here, rather than being
// re-justified at each of the several dozen call sites.
func (p *prompter) printf(format string, args ...any) {
	_, _ = fmt.Fprintf(p.out, format, args...)
}

func (p *prompter) println(args ...any) {
	_, _ = fmt.Fprintln(p.out, args...)
}

// section prints a heading, so a long --full run reads as a few labeled groups rather than one
// undifferentiated wall of questions.
func (p *prompter) section(title string) {
	if !p.asks() {
		return
	}
	p.printf("\n%s\n%s\n", title, strings.Repeat("-", len(title)))
}

// note prints a line of explanation attached to the question that follows it.
func (p *prompter) note(format string, args ...any) {
	if !p.asks() {
		return
	}
	p.printf("  %s\n", fmt.Sprintf(format, args...))
}

// ask puts a question with a default, and returns the default when the operator just presses Enter.
//
// preset is a value already supplied by a flag. When one is present the question is not asked at all —
// an operator who passed --listen-addr should not then be asked for the listen address.
func (p *prompter) ask(question, preset, fallback string) (string, error) {
	if preset != "" {
		return preset, nil
	}
	if !p.asks() {
		if err := p.unattended(); err != nil {
			return "", err
		}
		return fallback, nil
	}

	if fallback != "" {
		p.printf("%s [%s]: ", question, fallback)
	} else {
		p.printf("%s: ", question)
	}

	line, err := p.readLine()
	if err != nil {
		return "", err
	}
	if line == "" {
		return fallback, nil
	}
	return line, nil
}

// askRequired repeats a question until it gets a non-empty answer. Used where there is no safe default —
// a database password being the case that matters.
func (p *prompter) askRequired(question string) (string, error) {
	for {
		answer, err := p.ask(question, "", "")
		if err != nil {
			return "", err
		}
		if answer != "" {
			return answer, nil
		}
		p.println("  This one has no default and can't be left empty.")
	}
}

// askSecret reads a value without echoing it to the terminal.
func (p *prompter) askSecret(question, preset string, allowEmpty bool) (string, error) {
	if preset != "" {
		return preset, nil
	}
	if !p.asks() {
		if err := p.unattended(); err != nil {
			return "", err
		}
		// allowEmpty has to be honored here too, not only in the loop below. Returning "" unconditionally
		// meant a scripted run silently accepted a missing required secret — the S3 secret access key was
		// written as "" and the backend then refused to start on it, with nothing in the wizard's output
		// pointing at the cause. Mirrors askRequiredOr's message so the two read the same.
		if !allowEmpty {
			return "", fmt.Errorf("%s is required; pass it as a flag or environment variable when running "+
				"non-interactively", question)
		}
		return "", nil
	}

	for {
		p.printf("%s: ", question)
		secret, err := p.readPassword()
		// ReadPassword swallows the newline the operator typed, so the cursor is still on the prompt
		// line; without this the next question would be appended to it.
		p.println()
		if err != nil {
			return "", fmt.Errorf("reading %s: %w", question, err)
		}
		if secret != "" || allowEmpty {
			return secret, nil
		}
		p.println("  This one has no default and can't be left empty.")
	}
}

// askChoice constrains an answer to a fixed set of options.
func (p *prompter) askChoice(question string, options []string, preset, fallback string) (string, error) {
	if preset != "" {
		if !slices.Contains(options, preset) {
			return "", fmt.Errorf("%s: %q is not one of %s", question, preset, strings.Join(options, ", "))
		}
		return preset, nil
	}
	if !p.asks() {
		if err := p.unattended(); err != nil {
			return "", err
		}
		return fallback, nil
	}

	labeled := fmt.Sprintf("%s (%s)", question, strings.Join(options, "/"))
	for {
		answer, err := p.ask(labeled, "", fallback)
		if err != nil {
			return "", err
		}
		if slices.Contains(options, answer) {
			return answer, nil
		}
		p.printf("  Please answer one of: %s\n", strings.Join(options, ", "))
	}
}

// askBool asks a yes/no question.
func (p *prompter) askBool(question string, preset *bool, fallback bool) (bool, error) {
	if preset != nil {
		return *preset, nil
	}
	if !p.asks() {
		if err := p.unattended(); err != nil {
			return false, err
		}
		return fallback, nil
	}

	fallbackLabel := "no"
	if fallback {
		fallbackLabel = "yes"
	}
	for {
		answer, err := p.ask(question+" (yes/no)", "", fallbackLabel)
		if err != nil {
			return false, err
		}
		switch strings.ToLower(answer) {
		case "y", "yes", "true":
			return true, nil
		case "n", "no", "false":
			return false, nil
		}
		p.println("  Please answer yes or no.")
	}
}

// askPort asks for a TCP port and rejects anything outside the valid range.
func (p *prompter) askPort(question, preset string, fallback int) (int, error) {
	for {
		answer, err := p.ask(question, preset, strconv.Itoa(fallback))
		if err != nil {
			return 0, err
		}
		port, convErr := strconv.Atoi(answer)
		if convErr == nil && port > 0 && port <= 65535 {
			return port, nil
		}
		if preset != "" {
			// A bad value came from a flag, so re-asking would loop forever on the same input.
			return 0, fmt.Errorf("%s: %q is not a port number between 1 and 65535", question, answer)
		}
		p.println("  Please enter a port number between 1 and 65535.")
	}
}

// readLine reads one answer, mapping a closed stdin to the same actionable error as a non-terminal.
func (p *prompter) readLine() (string, error) {
	line, err := p.in.ReadString('\n')
	if err != nil {
		if errors.Is(err, io.EOF) && line == "" {
			return "", ErrNotATerminal
		}
		if !errors.Is(err, io.EOF) {
			return "", fmt.Errorf("reading input: %w", err)
		}
	}
	return strings.TrimSpace(line), nil
}
