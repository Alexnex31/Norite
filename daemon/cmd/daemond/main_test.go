package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The version a release build reports comes from a linker flag naming this exact symbol. goreleaser's
// default is `main.version`, lowercase, which would silently miss this package's `main.Version` and leave
// every shipped daemon logging "dev" — the one field a support conversation starts from.
//
// The CLI has the equivalent test for its own symbol path (cli/internal/cliapp/root_test.go); this is the
// daemon's half of the same guard.
func TestVersionIsWiredToTheReleaseLDFlags(t *testing.T) {
	config, err := os.ReadFile("../../../.goreleaser.yaml")
	if err != nil {
		t.Fatalf("reading the goreleaser config: %v", err)
	}

	// The `daemon` build stanza's ldflags.
	stanza := regexp.MustCompile(`(?m)^\s*-\s*id:\s*daemon\b[\s\S]*?(?:\n\s*-\s*id:|\z)`).Find(config)
	if stanza == nil {
		t.Fatal("the goreleaser config has no daemon build stanza")
	}
	if !strings.Contains(string(stanza), "-X main.Version=") {
		t.Errorf("the daemon build does not set main.Version, so every release reports \"dev\":\n%s", stanza)
	}
}

func TestDaemonBinaryNameMatchesWhatTheCLIInstalls(t *testing.T) {
	config, err := os.ReadFile("../../../.goreleaser.yaml")
	if err != nil {
		t.Fatalf("reading the goreleaser config: %v", err)
	}

	match := regexp.MustCompile(`(?m)^\s*-\s*id:\s*daemon\b[\s\S]*?^\s*binary:\s*(\S+)`).FindSubmatch(config)
	if match == nil {
		t.Fatal("the goreleaser config's daemon build has no binary name")
	}

	// `norite daemon install` finds this executable by looking for that exact name next to itself and then
	// on PATH (cli/internal/daemonctl/locate.go). The two live in different Go modules and cannot share a
	// constant, so a rename on either side has to be caught here instead.
	if got := string(match[1]); got != "norite-daemon" {
		t.Errorf("goreleaser builds the daemon as %q, but the CLI looks for \"norite-daemon\"", got)
	}
}
