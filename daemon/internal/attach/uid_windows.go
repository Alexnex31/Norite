// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package attach

// ownUID has no meaning on Windows, where peerUID reports nothing to compare it with.
func ownUID() int { return -1 }
