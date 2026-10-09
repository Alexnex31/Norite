// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !windows

package automation

import (
	"errors"
	"syscall"
)

// addressInUse reports whether a listen failed because something else holds the port.
func addressInUse(err error) bool { return errors.Is(err, syscall.EADDRINUSE) }
