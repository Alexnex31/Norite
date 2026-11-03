// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func loadVoice(t *testing.T, env map[string]string) (VoiceSettings, error) {
	t.Helper()
	withoutConfigFile(t)
	t.Setenv(envPrefix+"DATABASE_URL", validDSN)
	t.Setenv(envPrefix+"JWT_SECRET", testJWTSecret)
	for k, v := range env {
		t.Setenv(envPrefix+k, v)
	}
	cfg, err := Load("")
	if err != nil {
		return VoiceSettings{}, err
	}
	return cfg.Voice()
}

func TestVoiceDefaults(t *testing.T) {
	v, err := loadVoice(t, nil)
	require.NoError(t, err)
	assert.False(t, v.Enabled, "voice stays off until joining a call exists")
	assert.Equal(t, ":3479", v.MediaListen)
	assert.Equal(t, ":3478", v.TURNListen)
	assert.Empty(t, v.TURNTLSListen, "TLS is offered only with a certificate")
	assert.Equal(t, netip.MustParseAddr("127.0.0.1"), v.Relay)
	assert.Equal(t, netip.MustParseAddrPort("127.0.0.1:3479"), v.SFU, "an SFU on every interface is reached on loopback")
	assert.Empty(t, v.Public)
}

func TestVoiceSettingsFollowTheMediaAddress(t *testing.T) {
	v, err := loadVoice(t, map[string]string{
		"VOICE_MEDIA_LISTEN":     "10.0.0.5:4000",
		"VOICE_PUBLIC_ADDRESSES": " 203.0.113.7 ,2001:db8::7",
		"VOICE_TURN_TLS_CERT":    "/etc/norite/turn.pem",
		"VOICE_TURN_TLS_KEY":     "/etc/norite/turn.key",
		"VOICE_RELAY_PORTS":      "50000-50100",
	})
	require.NoError(t, err)
	assert.Equal(t, netip.MustParseAddr("10.0.0.5"), v.Relay)
	assert.Equal(t, netip.MustParseAddrPort("10.0.0.5:4000"), v.SFU)
	assert.Equal(t, []netip.Addr{netip.MustParseAddr("203.0.113.7"), netip.MustParseAddr("2001:db8::7")}, v.Public)
	assert.Equal(t, ":5349", v.TURNTLSListen)
	assert.Equal(t, [2]uint16{50000, 50100}, v.RelayPorts)
}

// A mistake in [voice] is refused at startup whether or not voice is on, and names the setting.
func TestVoiceSettingsAreCheckedEvenWhenOff(t *testing.T) {
	for name, c := range map[string]struct {
		env  map[string]string
		name string
	}{
		"no media port":             {map[string]string{"VOICE_MEDIA_LISTEN": "10.0.0.5"}, "NORITE_VOICE_MEDIA_LISTEN"},
		"a random media port":       {map[string]string{"VOICE_MEDIA_LISTEN": ":0"}, "NORITE_VOICE_MEDIA_LISTEN"},
		"a name for media":          {map[string]string{"VOICE_MEDIA_LISTEN": "voice.example.com:3479"}, "NORITE_VOICE_MEDIA_LISTEN"},
		"a bad TURN address":        {map[string]string{"VOICE_TURN_LISTEN": "3478"}, "NORITE_VOICE_TURN_LISTEN"},
		"a loopback public":         {map[string]string{"VOICE_PUBLIC_ADDRESSES": "127.0.0.1"}, "NORITE_VOICE_PUBLIC_ADDRESSES"},
		"a name as public":          {map[string]string{"VOICE_PUBLIC_ADDRESSES": "voice.example.com"}, "NORITE_VOICE_PUBLIC_ADDRESSES"},
		"a certificate alone":       {map[string]string{"VOICE_TURN_TLS_CERT": "/a.pem"}, "NORITE_VOICE_TURN_TLS_KEY"},
		"a key alone":               {map[string]string{"VOICE_TURN_TLS_KEY": "/a.key"}, "NORITE_VOICE_TURN_TLS_CERT"},
		"a bad TLS address":         {map[string]string{"VOICE_TURN_TLS_LISTEN": "5349", "VOICE_TURN_TLS_CERT": "/a.pem", "VOICE_TURN_TLS_KEY": "/a.key"}, "NORITE_VOICE_TURN_TLS_LISTEN"},
		"a relay of every address":  {map[string]string{"VOICE_RELAY_ADDRESS": "0.0.0.0"}, "NORITE_VOICE_RELAY_ADDRESS"},
		"a relay in another family": {map[string]string{"VOICE_MEDIA_LISTEN": "10.0.0.5:4000", "VOICE_RELAY_ADDRESS": "::1"}, "NORITE_VOICE_RELAY_ADDRESS"},
		"a backwards range":         {map[string]string{"VOICE_RELAY_PORTS": "60000-50000"}, "NORITE_VOICE_RELAY_PORTS"},
		"a range of one end":        {map[string]string{"VOICE_RELAY_PORTS": "50000"}, "NORITE_VOICE_RELAY_PORTS"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := loadVoice(t, c.env)
			require.Error(t, err)
			assert.Contains(t, err.Error(), c.name)
		})
	}
}
