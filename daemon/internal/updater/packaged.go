// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package updater

import (
	"io"
	"os"
	"path/filepath"
	"strings"
)

// MarkerName is the file a package installs to say which package manager installed it (ADR 0035 §4).
//
// One binary goes into every Linux package, so the binary cannot know which manager put it there. A package
// that can install a file of its own installs this one: /usr/lib/norite/packaged-by beside /usr/bin, or
// beside the binaries themselves. Its content is one of packagedBy's keys and nothing else.
const MarkerName = "packaged-by"

// maxMarker bounds the read. Every key is a short word; a file longer than this is not a marker.
const maxMarker = 64

// packagedBy says, for each package manager this project publishes to, who installed the daemon and what
// updates it. The daemon never writes a binary a package manager installed: two updaters on one file is a
// package manager that no longer knows what it installed. It checks, says a release is out, and names the
// command.
var packagedBy = map[string]string{
	"apt":      "Norite's APT repository installed it, so `sudo apt update && sudo apt upgrade` updates it",
	"rpm":      "Norite's RPM repository installed it, so `sudo dnf upgrade norite` updates it (`sudo zypper update norite` on openSUSE)",
	"pacman":   "Norite's pacman repository installed it, so `sudo pacman -Syu` updates it",
	"aur":      "the AUR installed it, so an AUR helper updates it (`yay -Syu`, for one)",
	"homebrew": "Homebrew installed it, so `brew upgrade norite` updates it",
	"scoop":    "Scoop installed it, so `scoop update norite` updates it",
	"winget":   "winget installed it, so `winget upgrade Norite.Norite` updates it",
	"nix":      "Nix installed it, so the configuration that installed it updates it",
}

// packageManager returns the reason for a daemon a package manager installed, or "" when nothing says one
// did. exe is the daemon's own path, resolved.
//
// The marker comes first, then the directories the three managers that cannot install one put a portable
// binary in: Homebrew's Caskroom, winget's Packages directory and the Nix store. A marker naming no manager
// this project publishes to is ignored rather than trusted, since the reason is shown to a person.
func packageManager(exe, goos string) string {
	if exe == "" {
		return ""
	}
	dir := filepath.Dir(exe)
	for _, marker := range []string{
		filepath.Join(dir, MarkerName),
		filepath.Join(dir, "..", "lib", "norite", MarkerName),
	} {
		if reason, ok := packagedBy[readMarker(marker)]; ok {
			return reason
		}
	}

	// goos rather than the build's own separator, so every OS's shape is testable from one machine.
	slashed := exe
	if goos == "windows" {
		slashed = strings.ToLower(strings.ReplaceAll(exe, `\`, "/"))
	}
	switch {
	case strings.Contains(slashed, "/Caskroom/"):
		return packagedBy["homebrew"]
	case goos == "windows" && strings.Contains(slashed, "/microsoft/winget/packages/"):
		return packagedBy["winget"]
	case strings.HasPrefix(slashed, "/nix/store/"):
		return packagedBy["nix"]
	}
	return ""
}

// readMarker returns a marker's word, or "" for anything that is not a small regular file. A named pipe
// at the path would otherwise hold the daemon's start for ever (M21: a file is whatever is at the path).
func readMarker(path string) string {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxMarker {
		return ""
	}
	f, err := os.Open(path) //nolint:gosec // a fixed name beside the daemon's own binary
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	body, err := io.ReadAll(io.LimitReader(f, maxMarker))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(body))
}
