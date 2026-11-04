// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package voiceworker

import (
	"runtime"
	"strings"
)

// passed is what of the daemon's environment the worker is given, and nothing else is.
//
// os/exec hands a child its parent's whole environment by default, and a daemon started by hand holds its
// shell's: NORITE_API_TOKEN, NORITE_PASSWORD, a cloud provider's keys. The worker, which from M29 decodes a
// stranger's audio, gets what reaching the audio system, the network and its own files needs. M29 adds what
// its capture library reads, and nothing else (its roadmap entry).
var passed = map[string][]string{
	"unix": {
		"HOME", "USER", "LOGNAME", "PATH", "LANG", "LANGUAGE", "TZ", "TMPDIR",
		// PipeWire and PulseAudio find their sockets through these, and the portal M32 may ask through D-Bus.
		"XDG_RUNTIME_DIR", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_DATA_DIRS",
		"DBUS_SESSION_BUS_ADDRESS", "PULSE_SERVER", "PULSE_RUNTIME_PATH", "PIPEWIRE_REMOTE",
		"PIPEWIRE_RUNTIME_DIR",
		// Pion verifies TURN over TLS against the system's roots, which these may name.
		"SSL_CERT_FILE", "SSL_CERT_DIR",
	},
	"windows": {
		"SystemRoot", "SystemDrive", "windir", "TEMP", "TMP", "USERPROFILE", "APPDATA", "LOCALAPPDATA",
		"HOMEDRIVE", "HOMEPATH", "USERNAME", "PATH", "PATHEXT", "PROCESSOR_ARCHITECTURE",
		"NUMBER_OF_PROCESSORS", "SSL_CERT_FILE", "SSL_CERT_DIR",
	},
}

// prefixes are variables passed by prefix: the locale's categories.
var prefixes = map[string][]string{"unix": {"LC_"}, "windows": nil}

// environment keeps from env what the worker is given. Windows compares names without case, as it does.
func environment(env []string) []string {
	key := "unix"
	if runtime.GOOS == "windows" {
		key = "windows"
	}
	same := func(a, b string) bool { return a == b }
	hasPrefix := strings.HasPrefix
	if key == "windows" {
		same = strings.EqualFold
		hasPrefix = func(s, p string) bool { return len(s) >= len(p) && strings.EqualFold(s[:len(p)], p) }
	}
	out := make([]string, 0, len(passed[key]))
	for _, kv := range env {
		name, _, ok := strings.Cut(kv, "=")
		if !ok || name == "" {
			continue
		}
		keep := false
		for _, n := range passed[key] {
			if same(name, n) {
				keep = true
				break
			}
		}
		for _, p := range prefixes[key] {
			keep = keep || hasPrefix(name, p)
		}
		if keep {
			out = append(out, kv)
		}
	}
	return out
}
