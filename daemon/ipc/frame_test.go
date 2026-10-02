// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package ipc

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/gatewayproto"
)

func TestAFrameRoundTrips(t *testing.T) {
	var buf bytes.Buffer
	sent, err := Encode(gatewayproto.OpHello, Hello{Version: "0.1.0"})
	require.NoError(t, err)
	require.NoError(t, WriteFrame(&buf, sent))

	n := binary.BigEndian.Uint32(buf.Bytes()[:4])
	assert.Equal(t, buf.Len()-4, int(n), "the prefix is the payload's length, big-endian")

	got, err := ReadFrame(&buf, MaxClientFrame)
	require.NoError(t, err)
	assert.Equal(t, gatewayproto.OpHello, got.Op)
	assert.Nil(t, got.S)
	assert.Nil(t, got.T)

	var hello Hello
	require.NoError(t, Decode(got, &hello))
	assert.Equal(t, "0.1.0", hello.Version)

	_, err = ReadFrame(&buf, MaxClientFrame)
	assert.ErrorIs(t, err, io.EOF, "a close between frames is the ordinary end")
}

// TestALengthOverTheLimitIsRefusedBeforeItIsRead is the bound that matters: the prefix is the peer's claim,
// and reading what it claims would let the peer choose how much this process allocates. The reader here
// holds the prefix and nothing else, so a ReadFrame that believed it would fail on EOF instead.
func TestALengthOverTheLimitIsRefusedBeforeItIsRead(t *testing.T) {
	var prefix [4]byte
	binary.BigEndian.PutUint32(prefix[:], MaxClientFrame+1)

	_, err := ReadFrame(bytes.NewReader(prefix[:]), MaxClientFrame)
	require.ErrorIs(t, err, ErrFrameTooLarge)

	binary.BigEndian.PutUint32(prefix[:], ^uint32(0))
	_, err = ReadFrame(bytes.NewReader(prefix[:]), MaxDaemonFrame)
	require.ErrorIs(t, err, ErrFrameTooLarge)
}

func TestMalformedFramesAreRefused(t *testing.T) {
	frame := func(payload string) io.Reader {
		var buf bytes.Buffer
		require.NoError(t, WriteEncoded(&buf, []byte(payload)))
		return &buf
	}

	_, err := ReadFrame(frame("{not json"), MaxClientFrame)
	assert.Error(t, err)

	_, err = ReadFrame(bytes.NewReader([]byte{0, 0, 0, 0}), MaxClientFrame)
	assert.Error(t, err, "an empty frame")

	truncated := []byte{0, 0, 0, 10, '{', '"'}
	_, err = ReadFrame(bytes.NewReader(truncated), MaxClientFrame)
	assert.ErrorIs(t, err, io.ErrUnexpectedEOF, "a close mid-frame is not the ordinary end")
	assert.False(t, errors.Is(err, io.EOF))
}

// TestDecodeRefusesAFieldItDoesNotDeclare: both ends come from one repository and match exactly under the
// 0.x rule, so an unknown field is a bug. In IDENTIFY it is also how a token would arrive, and is refused.
func TestDecodeRefusesAFieldItDoesNotDeclare(t *testing.T) {
	f := gatewayproto.Frame{Op: gatewayproto.OpIdentify, D: json.RawMessage(
		`{"token":"eyJ","properties":{"os":"linux","client":"norite","version":"dev"},"events":false}`)}
	var id Identify
	require.Error(t, Decode(f, &id))

	f.D = json.RawMessage(`{"properties":{"os":"linux","client":"norite","version":"dev"},"events":true} {}`)
	require.Error(t, Decode(f, &id), "trailing data")
}
