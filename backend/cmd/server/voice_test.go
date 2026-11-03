// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"net"
	"syscall"
	"testing"
	"time"

	"github.com/pion/stun/v4"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/internal/config"
)

// freeVoicePort is a loopback address free on UDP and TCP alike, as TURN's must be.
func freeVoicePort(t *testing.T) string {
	t.Helper()
	for range 20 {
		ln, err := net.Listen("tcp4", "127.0.0.1:0")
		require.NoError(t, err)
		addr := ln.Addr().String()
		_ = ln.Close()
		if pc, err := net.ListenPacket("udp4", addr); err == nil {
			_ = pc.Close()
			return addr
		}
	}
	t.Fatal("no port free on both")
	return ""
}

func udpFree(addr string) bool {
	pc, err := net.ListenPacket("udp4", addr)
	if err != nil {
		return false
	}
	_ = pc.Close()
	return true
}

// TestVoiceIsUpBeforeTheServerAnswersAndDownAfterItStops: with [voice] on, the media port is held and TURN
// answers a Binding request by the time /healthz answers anything, and on SIGTERM voice is stopped by the
// server rather than by its exit.
func TestVoiceIsUpBeforeTheServerAnswersAndDownAfterItStops(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping container-backed test in -short mode")
	}
	binary := buildServer(t)
	dsn := startPostgres(t)
	addr := reserveLocalAddr(t)
	media, turnAddr := freeVoicePort(t), freeVoicePort(t)

	server := startServer(t, binary, addr, dsn,
		"NORITE_VOICE_ENABLED=true",
		"NORITE_VOICE_MEDIA_LISTEN="+media,
		"NORITE_VOICE_TURN_LISTEN="+turnAddr,
	)
	healthz := "http://" + addr + "/api/v1/healthz"
	waitFor(t, 60*time.Second, "/healthz to answer", func() bool {
		_, _, err := getHealth(healthz)
		return err == nil
	})
	assert.False(t, udpFree(media), "the media port is not held")

	conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	target, err := net.ResolveUDPAddr("udp4", turnAddr)
	require.NoError(t, err)
	_, err = conn.WriteTo(stun.MustBuild(stun.TransactionID, stun.BindingRequest).Raw, target)
	require.NoError(t, err)
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 1500)
	n, _, err := conn.ReadFrom(buf)
	require.NoError(t, err, "TURN did not answer a Binding request; output:\n%s", server.output())
	var msg stun.Message
	require.NoError(t, stun.Decode(buf[:n], &msg))
	assert.Equal(t, stun.BindingSuccess, msg.Type)

	// A stop during migrations is a failed start by design, so the stop waits for ready.
	waitFor(t, 60*time.Second, "/healthz to return 200", func() bool {
		status, _, err := getHealth(healthz)
		return err == nil && status == 200
	})
	require.NoError(t, server.Process.Signal(syscall.SIGTERM))
	select {
	case err := <-server.wait:
		require.NoError(t, err, "output:\n%s", server.output())
	case <-time.After(30 * time.Second):
		t.Fatalf("the server did not stop; output:\n%s", server.output())
	}
	assert.Contains(t, server.output(), "voice stopped", "voice was left to the process's exit")
	assert.True(t, udpFree(media), "the media port is still held")
	assert.True(t, udpFree(turnAddr), "TURN's port is still held")
}

// TestAVoicePortInUseStopsTheStartBeforeMigrating: a voice port in use is a failed start, found before the
// schema is touched, as a taken HTTP port is.
func TestAVoicePortInUseStopsTheStartBeforeMigrating(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping container-backed test in -short mode")
	}
	binary := buildServer(t)
	dsn := startPostgres(t)
	taken, err := net.ListenPacket("udp4", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = taken.Close() }()

	server := startServer(t, binary, reserveLocalAddr(t), dsn,
		"NORITE_VOICE_ENABLED=true",
		"NORITE_VOICE_MEDIA_LISTEN="+taken.LocalAddr().String(),
		"NORITE_VOICE_TURN_LISTEN="+freeVoicePort(t),
	)
	select {
	case err := <-server.wait:
		require.Error(t, err)
		assert.Contains(t, server.output(), "could not start voice")
	case <-time.After(60 * time.Second):
		t.Fatalf("the server neither failed nor exited; output:\n%s", server.output())
	}
	assert.False(t, schemaMigrationsTableExists(t, dsn), "the schema was migrated by a start that failed")
}

// TestVoiceOffBindsNothing: with [voice] off, the ports it names stay free.
func TestVoiceOffBindsNothing(t *testing.T) {
	media, turnAddr := freeVoicePort(t), freeVoicePort(t)
	cfg := config.Config{VoiceMediaListen: media, VoiceTURNListen: turnAddr, VoiceTURNTLSListen: ":5349",
		VoiceRelayPerAddress: 64}
	st, err := startVoice(cfg, "dev", zerolog.Nop())
	require.NoError(t, err)
	assert.Nil(t, st)
	assert.True(t, udpFree(media))
	assert.True(t, udpFree(turnAddr))
}
