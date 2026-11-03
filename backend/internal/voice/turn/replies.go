// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package turn

import (
	"context"
	"encoding/binary"
	"net"
	"strconv"

	"github.com/pion/stun/v4"

	"github.com/Alexnex31/Norite/backend/internal/platform/ratelimit"
)

// replies bounds what TURN answers over UDP without authentication. The source of a UDP request can be
// forged, and every such answer is larger than the request that drew it: a Binding response carries the
// mapped address and a fingerprint, a challenge carries a nonce and the realm. Unbounded, the server
// would be a reflector, sending a multiple of what it receives to whoever an attacker names.
//
// Always the in-memory store, whatever the instance's rate limits use: this is counted per packet, and
// each TURN server answers for its own socket.
type replies struct {
	perDestination *ratelimit.Limiter
	total          *ratelimit.Limiter
}

func newReplies(lim Limits) (*replies, error) {
	per, err := ratelimit.New(ratelimit.Options{
		Rate:   strconv.Itoa(lim.RepliesPerSecond) + "-S",
		Bucket: "turn-replies",
	})
	if err != nil {
		return nil, err
	}
	total, err := ratelimit.New(ratelimit.Options{
		Rate:   strconv.Itoa(lim.RepliesTotalPerSecond) + "-S",
		Bucket: "turn-replies-total",
	})
	if err != nil {
		return nil, err
	}
	return &replies{perDestination: per, total: total}, nil
}

// allow counts one unauthenticated reply to addr. A store error cannot happen in memory; if it did, the
// reply is not sent, the opposite of HTTP's choice, since the cost of a missed reply is one retry.
func (r *replies) allow(addr net.Addr) bool {
	ctx := context.Background()
	res, err := r.total.Allow(ctx, "all")
	if err != nil || !res.Allowed {
		return false
	}
	res, err = r.perDestination.Allow(ctx, sourceKey(addr))
	return err == nil && res.Allowed
}

// replyConn is TURN's UDP socket as pion writes to it.
type replyConn struct {
	net.PacketConn
	s *Server
}

func (c *replyConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	if unauthenticatedReply(p) && !c.s.replies.allow(addr) {
		return len(p), nil
	}
	return c.PacketConn.WriteTo(p, addr)
}

// unauthenticatedReply is whether p is a STUN response carrying no message integrity: what the server
// sends to a request it has not authenticated. A response to an authenticated request is integrity-
// protected, and relayed data is ChannelData or an indication, neither of which is a response.
func unauthenticatedReply(p []byte) bool {
	const header = 20
	if !stun.IsMessage(p) || p[0]&0xc0 != 0 {
		return false
	}
	var class stun.MessageType
	class.ReadValue(binary.BigEndian.Uint16(p[0:2]))
	if class.Class != stun.ClassSuccessResponse && class.Class != stun.ClassErrorResponse {
		return false
	}
	// Walk the attributes for MESSAGE-INTEGRITY, either kind. The length is pion's own, from a message
	// it just built, but is bounded by what is there anyway.
	body := p[header:]
	if n := int(binary.BigEndian.Uint16(p[2:4])); n < len(body) {
		body = body[:n]
	}
	for len(body) >= 4 {
		t := stun.AttrType(binary.BigEndian.Uint16(body[0:2]))
		if t == stun.AttrMessageIntegrity || t == stun.AttrMessageIntegritySHA256 {
			return false
		}
		n := int(binary.BigEndian.Uint16(body[2:4]))
		n += (4 - n%4) % 4
		if 4+n > len(body) {
			break
		}
		body = body[4+n:]
	}
	return true
}
