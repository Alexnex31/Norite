// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !unix && !windows

package daemonctl

import "context"

// processExited cannot tell here, so it says the process was not seen to exit and the caller goes on to
// whatever stop it has left.
func processExited(context.Context, int) bool { return false }
