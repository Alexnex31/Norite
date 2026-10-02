// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package attach

import (
	"fmt"
	"net"

	"github.com/Microsoft/go-winio"

	"github.com/Alexnex31/Norite/daemon/ipc"
)

// Listen creates this account's pipe. stateDir is unused: a pipe lives in its own namespace.
//
// As the first instance — go-winio creates the first handle with FILE_CREATE — so a name another process
// already holds is an error here rather than a second instance of somebody else's pipe. With an owner-only
// DACL, because the default one lets Everyone and the anonymous account read a pipe. And refusing remote
// clients, which go-winio does for every pipe it makes.
func Listen(string) (net.Listener, error) {
	sid, err := ipc.UserSID()
	if err != nil {
		return nil, err
	}
	addr, err := ipc.Address()
	if err != nil {
		return nil, err
	}
	l, err := winio.ListenPipe(addr, &winio.PipeConfig{SecurityDescriptor: ipc.PipeSecurity(sid)})
	if err != nil {
		// The name is taken, by another daemon or a squatter: as permanent as a path that is too long, since
		// a restart meets the same holder.
		return nil, fmt.Errorf("%w: creating the attach pipe %s (is another process holding the name?): %w",
			ErrUnusable, addr, err)
	}
	return l, nil
}
