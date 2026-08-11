package cliapp

import (
	"bytes"
	"context"
	"io"
	"os"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v3"
)

// runArgs exercises the command tree exactly as a real invocation would, minus the process.
func runArgs(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	err := New(out, errOut).Run(context.Background(), append([]string{"norite"}, args...))
	return out.String(), errOut.String(), err
}

func TestHelpListsTheCommandTree(t *testing.T) {
	out, _, err := runArgs(t, "--help")
	require.NoError(t, err)

	assert.Contains(t, out, "instance", "the instance command group must be discoverable from the root")
	assert.Contains(t, out, "--"+JSONFlagName, "the global JSON flag must be documented")
}

func TestInstanceGroupListsInit(t *testing.T) {
	out, _, err := runArgs(t, "instance", "--help")
	require.NoError(t, err)
	assert.Contains(t, out, "init")
}

// The wizard's help is the only documentation an operator running it for the first time will read, so the
// flags that change what it does have to be visible there.
func TestInitHelpDocumentsItsModes(t *testing.T) {
	out, _, err := runArgs(t, "instance", "init", "--help")
	require.NoError(t, err)

	assert.Contains(t, out, "--full")
	assert.Contains(t, out, "--non-interactive")
	assert.Contains(t, out, "--output")
	assert.Contains(t, out, "--force")
	assert.Contains(t, out, "0600", "the help must say the file holds credentials")
}

// A password on the command line is readable from the process list by every other user on the machine.
// The flag stays, because some automation has nowhere better to put it, but the help must point at the
// environment variable instead.
func TestInitHelpSteersAwayFromPasswordsOnTheCommandLine(t *testing.T) {
	out, _, err := runArgs(t, "instance", "init", "--help")
	require.NoError(t, err)
	assert.Contains(t, out, "NORITE_DB_PASSWORD")
}

func TestVersionIsReported(t *testing.T) {
	out, _, err := runArgs(t, "--version")
	require.NoError(t, err)
	assert.Contains(t, out, Version)
}

func TestUnknownCommandIsAnError(t *testing.T) {
	_, _, err := runArgs(t, "definitely-not-a-command")
	require.Error(t, err)
}

// Shell completion is part of M2's scope rather than a later polish item. The generator is hidden from
// the help listing by the library's own default, so the check that means anything is running it.
func TestShellCompletionIsAvailable(t *testing.T) {
	for _, shell := range []string{"bash", "zsh", "fish"} {
		t.Run(shell, func(t *testing.T) {
			out, _, err := runArgs(t, "completion", shell)
			require.NoError(t, err)
			assert.NotEmpty(t, out, "completion must emit a script for %s", shell)
		})
	}
}

// Bare `norite` should introduce itself rather than fail — it is what someone types first.
func TestBareInvocationShowsHelp(t *testing.T) {
	out, _, err := runArgs(t)
	require.NoError(t, err)
	assert.Contains(t, out, "instance")
}

// A typo should point at what was probably meant instead of only saying no.
func TestTypoSuggestsTheRealCommand(t *testing.T) {
	_, errOut, err := runArgs(t, "instnace")
	require.Error(t, err)
	assert.Contains(t, errOut+err.Error(), "instance")
}

// The name the CLI calls itself must match the name it is actually installed as.
//
// These drifted apart once already: goreleaser shipped the binary as `norite` while every doc, the help
// output, and the error prefix said `app`, so anyone installing a release got a binary whose own help
// referred to a command they did not have. Nothing failed — it just quietly contradicted itself, which is
// why it is asserted here rather than left to review.
func TestRootNameMatchesTheShippedBinary(t *testing.T) {
	config, err := os.ReadFile("../../../.goreleaser.yaml")
	require.NoError(t, err)

	// The `cli` build stanza's binary: line.
	match := regexp.MustCompile(`(?m)^\s*-\s*id:\s*cli\b[\s\S]*?^\s*binary:\s*(\S+)`).FindSubmatch(config)
	require.NotNil(t, match, "the goreleaser config must have a cli build with a binary name")

	assert.Equal(t, string(match[1]), New(io.Discard, io.Discard).Name,
		"the root command's name and the shipped binary name must agree")
}

// The version reported by a release build comes from a linker flag naming this exact symbol path. A
// package move would silently leave every release reporting "dev".
func TestVersionIsWiredToTheReleaseLDFlags(t *testing.T) {
	config, err := os.ReadFile("../../../.goreleaser.yaml")
	require.NoError(t, err)

	assert.Contains(t, string(config), "github.com/Alexnex31/Norite/cli/internal/cliapp.Version=",
		"goreleaser must set cliapp.Version, or `norite --version` reports \"dev\" in every release")
}

func TestDaemonGroupIsMounted(t *testing.T) {
	out, _, err := runArgs(t, "--help")
	require.NoError(t, err)
	assert.Contains(t, out, "daemon", "the daemon command group must be discoverable from the root")

	out, _, err = runArgs(t, "daemon", "--help")
	require.NoError(t, err)
	for _, sub := range []string{"install", "uninstall", "start", "stop", "restart", "status"} {
		assert.Contains(t, out, sub, "`norite daemon --help` must list every subcommand")
	}
}

// urfave/cli's default handling of an ExitCoder prints the error and calls os.Exit from *inside* Run. That
// would make cmd/app/main.go — the one place this CLI decides exit codes — unreachable for any command that
// reports its result through one, starting with `norite daemon status`. It would also make such a command
// impossible to test, since the test binary would exit with it.
//
// This is exactly the kind of behavior that gets removed as an unexplained line during a refactor, so it is
// pinned here rather than only in a comment.
func TestExitCodersReachTheCallerInsteadOfExitingTheProcess(t *testing.T) {
	root := New(io.Discard, io.Discard)
	require.NotNil(t, root.ExitErrHandler,
		"without a no-op ExitErrHandler, urfave/cli calls os.Exit inside Run and main never sees the error")

	sentinel := cli.Exit("deliberate", 7)
	root.Commands = append(root.Commands, &cli.Command{
		Name:   "exit-probe",
		Hidden: true,
		Action: func(context.Context, *cli.Command) error { return sentinel },
	})

	// Reaching the assertion at all is half the test: the default handler would have ended the process here.
	err := root.Run(context.Background(), []string{"norite", "exit-probe"})

	var coder cli.ExitCoder
	require.ErrorAs(t, err, &coder, "the ExitCoder must travel back to the caller intact")
	assert.Equal(t, 7, coder.ExitCode(), "main maps this code onto the process exit status")
}
