// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package attach

import (
	"fmt"
	"net"
)

// checkPeer refuses a client running as another account, where the platform says who the client is.
func (s *Server) checkPeer(nc net.Conn) error {
	uid, ok, err := peerUID(nc)
	if err != nil {
		return fmt.Errorf("could not read the attach client's credentials: %w", err)
	}
	if ok && uid != s.wantUID {
		return fmt.Errorf("the client runs as uid %d and the daemon as %d", uid, s.wantUID)
	}
	return nil
}
