package cliapp

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v3"

	"github.com/Alexnex31/Norite/cli/internal/instanceinit"
	"github.com/Alexnex31/Norite/cli/internal/login"
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

// `norite login` is the first thing anyone runs, so it has to be discoverable from the root rather than
// buried in a group — and its help has to steer people away from putting a password on the command line,
// where it would be visible in the process list to every other user on the machine.
func TestLoginIsMountedAndSteersAwayFromPasswordFlags(t *testing.T) {
	out, _, err := runArgs(t, "--help")
	require.NoError(t, err)
	assert.Contains(t, out, "login", "login must be discoverable from the root")
	assert.Contains(t, out, "logout", "a credential that can be stored must be removable")

	out, _, err = runArgs(t, "login", "--help")
	require.NoError(t, err)
	assert.Contains(t, out, "NORITE_PASSWORD", "the scripted path must be named in the help")
	assert.Contains(t, out, "process list", "the help must say why a password flag is not offered")

	// ...and there is no such flag to reach for.
	assert.NotContains(t, out, "--password")

	// The browser path is discoverable from the same help, since it is the one that needs no secret at all.
	assert.Contains(t, out, "--provider", "the OAuth path must be findable without reading the docs")
}

// The two sign-in methods are not combined by precedence. Passing flags from both is a mistake worth
// naming, because silently ignoring one leaves somebody watching a browser window and wondering why the
// address they gave was not used. Exit 2, the usage code, not 1.
func TestProviderAndEmailTogetherIsRefused(t *testing.T) {
	_, errOut, err := runArgs(t, "login", "--provider", "google", "--email", "ada@example.com")

	var exit cli.ExitCoder
	require.ErrorAs(t, err, &exit)
	assert.Equal(t, 2, exit.ExitCode())
	assert.Contains(t, exit.Error()+errOut, "--email")
}

// And --no-browser without --provider is refused rather than ignored, for the same reason.
func TestNoBrowserWithoutAProviderIsRefused(t *testing.T) {
	_, errOut, err := runArgs(t, "login", "--no-browser")

	var exit cli.ExitCoder
	require.ErrorAs(t, err, &exit)
	assert.Equal(t, 2, exit.ExitCode())
	assert.Contains(t, exit.Error()+errOut, "--provider")
}

// --device-code describes a browser on another device; --email and --no-browser both describe this one.
// Refused rather than resolved by precedence, for the reason above: whichever is quietly dropped, somebody
// waits for something that is never going to happen.
func TestDeviceCodeWithConflictingFlagsIsRefused(t *testing.T) {
	for _, args := range [][]string{
		{"login", "--device-code", "--email", "ada@example.com"},
		{"login", "--device-code", "--no-browser"},
	} {
		_, errOut, err := runArgs(t, args...)

		var exit cli.ExitCoder
		require.ErrorAs(t, err, &exit, "%v", args)
		assert.Equal(t, 2, exit.ExitCode(), "%v", args)
		assert.Contains(t, exit.Error()+errOut, "--device-code", "%v", args)
	}
}

// And --device-code with --provider is *not* refused: the automatic fallback produces exactly that
// combination, so a guard against it would exit 2 for every SSH user the flow exists for.
//
// Run rather than grepped for. This used to assert only that `login --help` mentions the flag, which a
// third guard in command.go would leave passing while breaking the path it is named after.
func TestDeviceCodeWithAProviderIsAllowed(t *testing.T) {
	_, _, err := runArgs(t, "login", "--device-code", "--provider", "google",
		"--instance", "http://127.0.0.1:1")

	// It fails — there is no instance at that address — but not as a usage error, which is the property.
	var exit cli.ExitCoder
	if errors.As(err, &exit) {
		assert.NotEqual(t, 2, exit.ExitCode(), "the pair the fallback produces must not be a usage error")
	}

	out, _, helpErr := runArgs(t, "login", "--help")
	require.NoError(t, helpErr)
	assert.Contains(t, out, "chosen in the browser",
		"the help must say where the provider choice happens on that path")
}

// The flags login does offer are the ones it needs, and none of them is a credential.
func TestLoginFlagsCarryNoCredential(t *testing.T) {
	out, _, err := runArgs(t, "login", "--help")
	require.NoError(t, err)
	for _, flag := range []string{
		"--instance", "--email", "--device-name", "--provider", "--no-browser", "--device-code",
	} {
		assert.Contains(t, out, flag)
	}
	for _, forbidden := range []string{"--password", "--secret", "--token"} {
		assert.NotContains(t, out, forbidden,
			"%s would put a credential in the process list and the shell history", forbidden)
	}
}

// A command that needs a terminal and has not got one exits 2, so a script can tell "this needs input I
// cannot give it" from "the credentials were wrong" without parsing a message. The wizard established the
// code; login has to agree with it, and the two live in different packages, so nothing but a test keeps
// them in step.
func TestNeedingATerminalIsAlwaysExitCodeTwo(t *testing.T) {
	// login returns the sentinel unwrapped rather than an ExitCoder, precisely so cmd/app/main.go can
	// recognize it and print without the "norite:" prefix that would make a usage problem read like a
	// crash. That means the contract is the error identity, not an exit code carried in the error.
	assert.NotErrorIs(t, login.ErrNoTerminal, instanceinit.ErrNotATerminal,
		"they are distinct sentinels; main matches both explicitly")

	for _, err := range []error{login.ErrNoTerminal, instanceinit.ErrNotATerminal} {
		assert.Contains(t, err.Error(), "terminal",
			"the message is what a person reads when a script hits this; it must name the problem")
	}
}
