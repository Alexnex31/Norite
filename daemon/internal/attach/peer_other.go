// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !linux && !darwin

package attach

import "net"

// peerUID is not read on this platform. On Windows the pipe's owner-only DACL is the whole of the check, as
// the directory's mode is the whole of it on a Unix this package does not ask: the uid check is defense in
// depth where the platform offers it, not the boundary.
func peerUID(net.Conn) (uid int, ok bool, err error) { return 0, false, nil }
