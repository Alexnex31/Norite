// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package voicetest

import (
	"os"
	"path"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The harness's provisioning endpoint authenticates nobody: it is the part M33 builds. So no release may
// build either of its programs, and this holds every build goreleaser makes to that.
func TestNoReleaseBuildsTheHarness(t *testing.T) {
	raw, err := os.ReadFile("../../../../.goreleaser.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Builds []struct {
			ID   string `yaml:"id"`
			Dir  string `yaml:"dir"`
			Main string `yaml:"main"`
		} `yaml:"builds"`
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Builds) == 0 {
		t.Fatal("no builds read; the test would pass for nothing")
	}
	for _, b := range cfg.Builds {
		built := path.Clean(path.Join(b.Dir, b.Main))
		if b.Main == "" || strings.Contains(built, "voicetest") {
			t.Fatalf("build %q builds %q", b.ID, built)
		}
	}
}
