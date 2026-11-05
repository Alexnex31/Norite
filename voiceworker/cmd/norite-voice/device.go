// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !norite_noaudio

package main

import "github.com/Alexnex31/Norite/voiceworker/internal/audio"

// device is the system's microphone and speakers.
func device() audio.Device { return audio.System() }
