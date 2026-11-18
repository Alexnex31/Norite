// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"net"
	"net/url"

	"github.com/rs/zerolog"

	"github.com/Alexnex31/Norite/backend/internal/config"
	"github.com/Alexnex31/Norite/backend/internal/voice"
)

// startVoice starts the SFU and TURN when [voice] is on, and returns nil when it is off: nothing is bound
// and nothing runs, which is M37's opt-out from the start. Op 4 provisions calls on it from M33.
func startVoice(cfg config.Config, version string, logger zerolog.Logger) (*voice.Stack, error) {
	v, err := cfg.Voice()
	if err != nil || !v.Enabled {
		return nil, err
	}
	st, err := voice.StartStack(voice.StackOptions{
		MediaListen:     v.MediaListen,
		Public:          v.Public,
		TURNListen:      v.TURNListen,
		TURNTLSListen:   v.TURNTLSListen,
		TLSCert:         v.TLSCert,
		TLSKey:          v.TLSKey,
		Relay:           v.Relay,
		SFU:             v.SFU,
		OtherRelay:      v.OtherRelay,
		OtherSFU:        v.OtherSFU,
		RelayPorts:      v.RelayPorts,
		RelayPerAddress: v.RelayPerAddress,
		SigningKey:      []byte(cfg.JWTSecret),
		Version:         version,
		Logger:          logger,
	})
	if err != nil {
		return nil, err
	}
	event := logger.Info().Str("media", v.MediaListen).Str("turn", v.TURNListen)
	if v.TURNTLSListen != "" {
		event = event.Str("turn_tls", v.TURNTLSListen)
	}
	event.Msg("voice is on: the SFU and TURN are listening")
	if len(turnURLs(cfg, v)) == 0 {
		logger.Warn().Msg("no [http] public_base_url: clients are offered no TURN relay, so a network " +
			"that refuses UDP to the SFU cannot join a call")
	}
	return st, nil
}

// turnURLs are where clients are told TURN is: the instance's own host name, from public_base_url, at
// TURN's ports, over UDP and TCP, and over TLS when a certificate is configured. The name rather than an
// address, because TURN over TLS is verified against the certificate's name, and a certificate for the
// instance is what an operator has. None without public_base_url, which is configured and never derived
// (M5): a host read off a request is whatever the request said.
func turnURLs(cfg config.Config, v config.VoiceSettings) []string {
	base, err := url.Parse(cfg.PublicBaseURL)
	if err != nil || base.Hostname() == "" {
		return nil
	}
	host := base.Hostname()
	_, port, err := net.SplitHostPort(v.TURNListen)
	if err != nil {
		return nil
	}
	at := net.JoinHostPort(host, port)
	urls := []string{"turn:" + at + "?transport=udp", "turn:" + at + "?transport=tcp"}
	if v.TURNTLSListen != "" {
		if _, tlsPort, err := net.SplitHostPort(v.TURNTLSListen); err == nil {
			urls = append(urls, "turns:"+net.JoinHostPort(host, tlsPort)+"?transport=tcp")
		}
	}
	return urls
}
