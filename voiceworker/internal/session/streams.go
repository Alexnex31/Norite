// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"io"
	"sync"
	"time"

	"github.com/pion/transport/v5/packetio"
)

// The voice server holds the call's SRTP keys, so whatever it encrypts is decrypted. For every SSRC it
// sends that the answer did not declare, SRTP opens a read stream with a buffer of its own (a megabyte by
// default), and Pion keeps it unread until the call ends: a server sending to a new SSRC with every packet
// grows the worker's memory without limit. M26 found it in the SFU, from the other side
// (backend/internal/voice/streams.go), and M28's /security-sweep carried it here for M33, once the server is
// an instance's.
//
// Every stream's buffer comes from the call's own factory, which counts them. Past what the call accounts
// for, the stream gets a closed buffer, which SRTP drops packets into, and the call ends.

// Buffer limits per stream. A declared stream is read as fast as it arrives; these bound one nobody reads.
const (
	rtpBuffer  = 128 << 10
	rtcpBuffer = 32 << 10
)

// maxStreams is how many streams a call may have opened: an RTP and an RTCP stream for each slot, an RTCP
// stream for the track it sends, and a few for what Pion opens of its own (probes on SSRC 0, feedback
// naming no media). The SFU's own count, with the sides swapped.
const maxStreams = 2*Slots + 1 + 8

// streams counts a call's streams.
type streams struct {
	mu    sync.Mutex
	count int
	// over ends the call, once.
	over func()
}

// buffer is the call's buffer factory. Called from inside SRTP, so it ends the call on a goroutine of its
// own rather than wait on the one calling it.
func (s *streams) buffer(kind packetio.BufferPacketType, _ uint32) io.ReadWriteCloser {
	s.mu.Lock()
	s.count++
	over := s.count > maxStreams
	s.mu.Unlock()

	b := streamBuffer{packetio.NewBuffer()}
	if over {
		_ = b.Close()
		go s.over()
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
