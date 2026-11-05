// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build norite_noaudio

package main

import "github.com/Alexnex31/Norite/voiceworker/internal/audio"

// device is none: a worker built with norite_noaudio carries calls without audio, so the daemon's end-to-end
// test can start real workers on any OS without opening the machine's microphone. No release passes the tag
// (release_test.go).
func device() audio.Device { return nil }
