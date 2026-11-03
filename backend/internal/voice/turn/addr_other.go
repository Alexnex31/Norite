// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !windows

package turn

import (
	"errors"
	"syscall"
)

// addressNotHere is whether err is a bind refused because the host has no such address.
func addressNotHere(err error) bool { return errors.Is(err, syscall.EADDRNOTAVAIL) }
