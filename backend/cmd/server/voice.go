// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"github.com/rs/zerolog"

	"github.com/Alexnex31/Norite/backend/internal/config"
	"github.com/Alexnex31/Norite/backend/internal/voice"
)

// startVoice starts the SFU and TURN when [voice] is on, and returns nil when it is off: nothing is bound
// and nothing runs, which is M37's opt-out from the start. Nothing provisions a call until op 4 exists
// (M33), so until then an instance with voice on holds open ports a client can reach TURN and STUN on,
// and an SFU that admits nobody.
func startVoice(cfg config.Config, version string, logger zerolog.Logger) (*voice.Stack, error) {
	v, err := cfg.Voice()
	if err != nil || !v.Enabled {
		return nil, err
	}
	st, err := voice.StartStack(voice.StackOptions{
		MediaListen:   v.MediaListen,
		Public:        v.Public,
		TURNListen:    v.TURNListen,
		TURNTLSListen: v.TURNTLSListen,
		TLSCert:       v.TLSCert,
		TLSKey:        v.TLSKey,
		Relay:         v.Relay,
		SFU:           v.SFU,
		RelayPorts:    v.RelayPorts,
		SigningKey:    []byte(cfg.JWTSecret),
		Version:       version,
		Logger:        logger,
	})
	if err != nil {
		return nil, err
	}
	event := logger.Info().Str("media", v.MediaListen).Str("turn", v.TURNListen)
	if v.TURNTLSListen != "" {
		event = event.Str("turn_tls", v.TURNTLSListen)
	}
	event.Msg("voice is on: the SFU and TURN are listening")
	return st, nil
}
