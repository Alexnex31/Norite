// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package automation

import (
	"errors"
	"syscall"
)

// wsaeAddrInUse is WSAEADDRINUSE, which package syscall does not name.
const wsaeAddrInUse syscall.Errno = 10048

// addressInUse reports whether a listen failed because something else holds the port, which Windows
// reports as a Winsock error of its own.
func addressInUse(err error) bool { return errors.Is(err, wsaeAddrInUse) }
