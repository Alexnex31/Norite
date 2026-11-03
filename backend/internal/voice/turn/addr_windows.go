// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package turn

import (
	"errors"
	"syscall"
)

// wsaeaddrnotavail is Winsock's code for an address the host does not have. syscall.EADDRNOTAVAIL is an
// invented value on Windows, which no socket call returns.
const wsaeaddrnotavail = syscall.Errno(10049)

// addressNotHere is whether err is a bind refused because the host has no such address.
func addressNotHere(err error) bool { return errors.Is(err, wsaeaddrnotavail) }
