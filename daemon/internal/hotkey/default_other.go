// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !linux && !windows && !darwin

package hotkey

// Default is a system Norite offers no key on.
func Default() Binder { return Unavailable("this system offers Norite no push-to-talk key") }
