// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package ipc

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/Alexnex31/Norite/backend/gatewayproto"
)

// ErrFrameTooLarge is a frame whose declared length exceeds the reader's bound. Nothing past the length
// prefix has been read, so the stream cannot be resynchronised and the connection has to close.
var ErrFrameTooLarge = errors.New("ipc: frame exceeds the size limit")

// WriteFrame writes one frame: its JSON behind a 4-byte big-endian length, in a single Write so a frame
// is never interleaved with another writer's on a stream that allows concurrent writes.
func WriteFrame(w io.Writer, f gatewayproto.Frame) error {
	payload, err := json.Marshal(f)
	if err != nil {
		return fmt.Errorf("ipc: encoding a frame: %w", err)
	}
	return WriteEncoded(w, payload)
}

// WriteEncoded writes an already-encoded frame. The fan-out encodes a dispatch once and writes the same
// bytes to every client, which is why this exists apart from WriteFrame.
func WriteEncoded(w io.Writer, payload []byte) error {
	if uint64(len(payload)) > uint64(^uint32(0)) {
		return ErrFrameTooLarge
	}
	buf := make([]byte, 4+len(payload))
	binary.BigEndian.PutUint32(buf, uint32(len(payload)))
	copy(buf[4:], payload)
	_, err := w.Write(buf)
	return err
}

// ReadFrame reads one frame, refusing one longer than limit before allocating for it: the length prefix is
// the peer's claim, and believing it would let the peer choose how much memory this process reserves.
//
// io.EOF means the peer closed between frames, the ordinary end of a connection. A close mid-frame is
// io.ErrUnexpectedEOF.
func ReadFrame(r io.Reader, limit int) (gatewayproto.Frame, error) {
	var prefix [4]byte
	if _, err := io.ReadFull(r, prefix[:]); err != nil {
		return gatewayproto.Frame{}, err
	}
	n := binary.BigEndian.Uint32(prefix[:])
	if n == 0 {
		return gatewayproto.Frame{}, errors.New("ipc: an empty frame")
	}
	if uint64(n) > uint64(limit) {
		return gatewayproto.Frame{}, ErrFrameTooLarge
	}

	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return gatewayproto.Frame{}, err
	}

	var f gatewayproto.Frame
	if err := json.Unmarshal(payload, &f); err != nil {
		return gatewayproto.Frame{}, fmt.Errorf("ipc: a frame that is not JSON: %w", err)
	}
	return f, nil
}

// Encode builds a frame with op and payload d, s and t null: every frame but a dispatch.
func Encode(op gatewayproto.Opcode, d any) (gatewayproto.Frame, error) {
	raw, err := json.Marshal(d)
	if err != nil {
		return gatewayproto.Frame{}, fmt.Errorf("ipc: encoding op %d: %w", op, err)
	}
	return gatewayproto.Frame{Op: op, D: raw}, nil
}

// Decode reads a frame's payload into v, refusing a field v does not declare. Both ends are built from one
// repository and must match exactly under the 0.x version rule, so an unknown field is a bug to surface, not
// a newer peer to tolerate.
func Decode(f gatewayproto.Frame, v any) error {
	dec := json.NewDecoder(bytes.NewReader(f.D))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("ipc: op %d's payload: %w", f.Op, err)
	}
	if dec.More() {
		return fmt.Errorf("ipc: op %d's payload has trailing data", f.Op)
	}
	return nil
}
