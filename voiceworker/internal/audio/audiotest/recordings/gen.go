// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build ignore

// gen encodes a recording for package recordings: 48 kHz mono s16 little-endian samples on stdin, Opus
// packets each behind a 2-byte big-endian length on stdout. testdata/SOURCES.md has the commands that made
// the samples from the downloads. From voiceworker/:
//
//	go run ./internal/audio/audiotest/recordings/gen.go <near.raw >internal/audio/audiotest/recordings/testdata/near.opus
package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/Alexnex31/Norite/voiceworker/internal/opus"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "gen:", err)
		os.Exit(1)
	}
}

func run() error {
	ctx := context.Background()
	c, err := opus.New(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close(ctx) }()
	// Near transparent, and nothing left out: no DTX, which would send a pause as nothing.
	enc, err := c.NewEncoder(ctx, opus.EncoderOptions{Bitrate: 96000, Complexity: 10})
	if err != nil {
		return err
	}
	in := bufio.NewReader(os.Stdin)
	out := bufio.NewWriter(os.Stdout)
	raw := make([]byte, 2*opus.FrameSamples)
	pcm := make([]int16, opus.FrameSamples)
	pkt := make([]byte, opus.MaxPacket)
	for {
		if _, err := io.ReadFull(in, raw); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return fmt.Errorf("the input is not whole 20 ms frames: %w", err)
		}
		for i := range pcm {
			pcm[i] = int16(binary.LittleEndian.Uint16(raw[2*i:])) //nolint:gosec // a sample's bits
		}
		n, err := enc.Encode(pcm, pkt)
		if err != nil {
			return err
		}
		if err := binary.Write(out, binary.BigEndian, uint16(n)); err != nil { //nolint:gosec // at most MaxPacket
			return err
		}
		if _, err := out.Write(pkt[:n]); err != nil {
			return err
		}
	}
	return out.Flush()
}
