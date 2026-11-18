// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/transport/v5/packetio"
	"github.com/stretchr/testify/require"
)

// The call's streams are counted: up to what its slots and its track account for each is a bounded buffer,
// and past it a closed one, and the call ends, once per stream past it and never from SRTP's own goroutine.
func TestStreamsPastTheCallsAreRefused(t *testing.T) {
	var ended atomic.Int32
	s := &streams{over: func() { ended.Add(1) }}
	for i := range maxStreams {
		b := s.buffer(packetio.RTPBufferPacket, uint32(i)).(streamBuffer)
		_, err := b.Write(make([]byte, 100))
		require.NoError(t, err, "stream %d is within the call's", i)
		_, err = b.Write(make([]byte, rtpBuffer))
		require.Error(t, err, "stream %d is unbounded", i)
	}
	require.Zero(t, ended.Load())

	b := s.buffer(packetio.RTPBufferPacket, 9999).(streamBuffer)
	_, err := b.Write(make([]byte, 100))
	require.Error(t, err, "a stream past the call's took a packet")
	require.Eventually(t, func() bool { return ended.Load() == 1 }, time.Second, time.Millisecond)

	c := s.buffer(packetio.RTCPBufferPacket, 1).(streamBuffer)
	_, err = c.Write(make([]byte, 100))
	require.Error(t, err)
}
