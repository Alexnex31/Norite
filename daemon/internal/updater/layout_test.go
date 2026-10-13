// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package updater

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The updater restates what a release publishes: the client archive's name, the two programs in it, the
// checksums file and its bundle, and the workflow whose identity signs them. Those are decided in
// .goreleaser.yaml and by release.yml's own name, and a rename there passes every other gate while every
// installed daemon refuses every later release, the one that would fix it included.
func TestTheReleaseLayoutIsWhatTheReleaseConfigurationPublishes(t *testing.T) {
	read := func(path string) string {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	config := read("../../../.goreleaser.yaml")

	release := mustTag(t, "v1.2.3")
	for platform, want := range map[[2]string]string{
		{"linux", "amd64"}:   "norite_1.2.3_linux_amd64.tar.gz",
		{"windows", "arm64"}: "norite_1.2.3_windows_arm64.zip",
	} {
		if got := archiveName(release, platform[0], platform[1]); got != want {
			t.Errorf("archiveName(%v) = %s", platform, got)
		}
	}
	// The client archive's stanza: the template those names are instances of, tar.gz, and zip on Windows.
	stanza := regexp.MustCompile(`(?s)name_template: >-\s+norite_\{\{ \.Version \}\}_\{\{ \.Os \}\}_\{\{ \.Arch \}\}\s.*?files:`).FindString(config)
	if stanza == "" {
		t.Fatal("no archive in .goreleaser.yaml is named norite_{{ .Version }}_{{ .Os }}_{{ .Arch }}")
	}
	before := config[:strings.Index(config, stanza)]
	formats := before[strings.LastIndex(before, "formats:"):]
	if !strings.HasPrefix(formats, "formats: [tar.gz]") || !regexp.MustCompile(`goos: windows\s+formats: \[zip\]`).MatchString(stanza) {
		t.Errorf("the client archive is not tar.gz with zip on Windows:\n%s%s", formats, stanza)
	}
	for _, binary := range []string{cliName, daemonName} {
		if !regexp.MustCompile(`(?m)^\s+binary: ` + binary + `$`).MatchString(config) {
			t.Errorf(".goreleaser.yaml builds no binary named %s", binary)
		}
	}
	if !strings.Contains(config, `name_template: "`+checksumsName+`"`) {
		t.Errorf(".goreleaser.yaml's checksums are not named %s", checksumsName)
	}
	if bundleName != checksumsName+".sigstore.json" || !strings.Contains(config, `signature: "${artifact}.sigstore.json"`) {
		t.Errorf("the bundle is %s, and .goreleaser.yaml signs to something else", bundleName)
	}

	// The identity is the workflow file's own path, and the footer's verify command names the same one.
	const prefix = "https://github.com/Alexnex31/Norite/"
	workflow := strings.TrimSuffix(strings.TrimPrefix(releaseWorkflow, prefix), "@refs/tags/")
	if !strings.HasPrefix(releaseWorkflow, prefix) || workflow == releaseWorkflow {
		t.Fatalf("the identity %q is not this repository's workflow at a tag", releaseWorkflow)
	}
	read("../../../" + workflow)
	if !strings.Contains(config, "--certificate-identity '"+releaseWorkflow+"{{ .Tag }}'") {
		t.Errorf(".goreleaser.yaml's footer does not name %s", workflow)
	}
}
