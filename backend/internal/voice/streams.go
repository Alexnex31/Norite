// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package voice

import (
	"io"
	"time"

	"github.com/pion/transport/v5/packetio"
	"github.com/pion/webrtc/v4"
)

// An admitted peer holds the SRTP keys, so whatever it encrypts is decrypted. For every SSRC it sends
// that its offer did not declare, SRTP opens a read stream with a buffer of its own (a megabyte, by
// default), and Pion keeps the stream until the connection closes, unread. Pion bounds the goroutines
// that probe such streams and nothing else, and those packets never reach the per-track meter. So a peer
// sending to a new SSRC with every packet grows the server's memory without limit, and the self-hosted
// SFU shares norite-server's process. Found by /security-sweep, by reading Pion's
// undeclaredRTPMediaProcessor and srtp's getOrCreateReadStream.
//
// Every stream's buffer comes from the connection's own factory, which counts them. Past what the
// connection's offer accounts for, the stream gets a closed buffer, which SRTP drops packets into, and the
// connection is closed.

// Buffer limits per stream. A declared stream is read as fast as it arrives; these bound one nobody reads.
const (
	rtpBuffer  = 128 << 10
	rtcpBuffer = 32 << 10
)

// streamsFor is how many streams a connection with these tracks and slots may open: an RTP and an RTCP
// stream for each track it sends, an RTCP stream for each slot, and a few for what Pion opens of its own
// (bandwidth probes on SSRC 0, feedback naming no media).
func streamsFor(tracks, slots int) int {
	return 2*tracks + slots + 8
}

// apiFor builds the API a connection's peer connection comes from, with the connection's buffer factory.
func (s *SFU) apiFor(c *conn) *webrtc.API {
	se := s.se
	se.BufferFactory = c.buffer
	return webrtc.NewAPI(webrtc.WithMediaEngine(s.media), webrtc.WithInterceptorRegistry(s.reg),
		webrtc.WithSettingEngine(se))
}

// buffer is the connection's buffer factory. Called from inside SRTP, so it never closes the connection
// itself: that would wait on the goroutine calling it.
func (c *conn) buffer(kind packetio.BufferPacketType, _ uint32) io.ReadWriteCloser {
	c.mu.Lock()
	c.streams++
	over := c.streams > c.maxStreams
	c.mu.Unlock()

	b := streamBuffer{packetio.NewBuffer()}
	if over {
		_ = b.Close()
		go c.close("sending on more SSRCs than its offer declared")
		return b
	}
	if kind == packetio.RTCPBufferPacket {
		b.SetLimitSize(rtcpBuffer)
	} else {
		b.SetLimitSize(rtpBuffer)
	}
	return b
}

// streamBuffer is a packetio.Buffer as SRTP takes one, as SRTP's own default adapts it: plain Read and
// Write for io.ReadWriteCloser, and the attribute-carrying pair and the deadline SRTP looks for.
type streamBuffer struct{ *packetio.Buffer }

var _ io.ReadWriteCloser = streamBuffer{}

func (b streamBuffer) Read(p []byte) (int, error) {
	n, _, err := b.Buffer.Read(p, nil)
	return n, err
}

func (b streamBuffer) Write(p []byte) (int, error) { return b.Buffer.Write(p, nil) }

func (b streamBuffer) ReadWithAttributes(p []byte, a packetio.Attributes) (int, packetio.Attributes, error) {
	return b.Buffer.Read(p, a)
}

func (b streamBuffer) WriteWithAttributes(p []byte, a packetio.Attributes) (int, error) {
	return b.Buffer.Write(p, a)
}

func (b streamBuffer) SetReadDeadline(t time.Time) error { return b.Buffer.SetReadDeadline(t) }
