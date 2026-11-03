// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
)

// VoiceSettings are the [voice] settings, parsed. Config keeps them as the strings they were given, so
// that every error can name the setting it came from; this is what the server builds the stack from.
type VoiceSettings struct {
	Enabled     bool
	MediaListen string
	Public      []netip.Addr
	TURNListen  string
	// TURNTLSListen is empty when no certificate is configured: TLS is offered only with one.
	TURNTLSListen string
	TLSCert       string
	TLSKey        string
	// Relay is where relay sockets are bound, and SFU the media address as the relay reaches it.
	Relay      netip.Addr
	SFU        netip.AddrPort
	RelayPorts [2]uint16
}

// Voice parses and checks the [voice] settings. They are checked whether or not voice is enabled, so a
// mistake is found when the file is written rather than the day somebody turns voice on.
func (c Config) Voice() (VoiceSettings, error) {
	bad := func(field, format string, a ...any) error {
		name := envVarFor(field)
		if c.SourcePath != "" {
			name = fmt.Sprintf("%s (%s in %s)", name, fileKeyFor(field), c.SourcePath)
		}
		return fmt.Errorf("config: invalid configuration: %s: %s", name, fmt.Sprintf(format, a...))
	}
	v := VoiceSettings{Enabled: c.VoiceEnabled, MediaListen: c.VoiceMediaListen, TURNListen: c.VoiceTURNListen}

	media, err := listenAddr(c.VoiceMediaListen)
	if err != nil || c.VoiceMediaListen == "" {
		return VoiceSettings{}, bad("VoiceMediaListen", "must be an address and a port, e.g. \":3479\"")
	}
	if _, err := listenAddr(c.VoiceTURNListen); err != nil {
		return VoiceSettings{}, bad("VoiceTURNListen", "must be an address and a port, e.g. \":3478\"")
	}

	for part := range strings.SplitSeq(c.VoicePublicAddresses, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		a, err := netip.ParseAddr(part)
		if err != nil || a.Zone() != "" || a.IsUnspecified() || a.IsLoopback() || a.IsMulticast() {
			return VoiceSettings{}, bad("VoicePublicAddresses", "%q is not an address a client can reach", part)
		}
		v.Public = append(v.Public, a.Unmap())
	}

	switch {
	case (c.VoiceTURNTLSCert == "") != (c.VoiceTURNTLSKey == ""):
		field := "VoiceTURNTLSKey"
		if c.VoiceTURNTLSCert == "" {
			field = "VoiceTURNTLSCert"
		}
		return VoiceSettings{}, bad(field, "a certificate and its key are set together, or neither is")
	case c.VoiceTURNTLSCert != "":
		if _, err := listenAddr(c.VoiceTURNTLSListen); err != nil || c.VoiceTURNTLSListen == "" {
			return VoiceSettings{}, bad("VoiceTURNTLSListen", "must be an address and a port, e.g. \":5349\"")
		}
		v.TURNTLSListen, v.TLSCert, v.TLSKey = c.VoiceTURNTLSListen, c.VoiceTURNTLSCert, c.VoiceTURNTLSKey
	}

	// The relay sits on the SFU's host and reaches the SFU there: at the media address when that names
	// one, and on loopback when the SFU listens on every interface.
	v.Relay = netip.MustParseAddr("127.0.0.1")
	if media.Addr().IsValid() && !media.Addr().IsUnspecified() {
		v.Relay = media.Addr()
	}
	if c.VoiceRelayAddress != "" {
		a, err := netip.ParseAddr(c.VoiceRelayAddress)
		if err != nil || a.Zone() != "" || a.IsUnspecified() || a.IsMulticast() {
			return VoiceSettings{}, bad("VoiceRelayAddress", "must be one address of this host")
		}
		v.Relay = a.Unmap()
	}
	sfu := v.Relay
	if media.Addr().IsValid() && !media.Addr().IsUnspecified() {
		sfu = media.Addr()
	}
	if sfu.Is4() != v.Relay.Is4() {
		return VoiceSettings{}, bad("VoiceRelayAddress", "must be in the media address's family")
	}
	v.SFU = netip.AddrPortFrom(sfu, media.Port())

	if c.VoiceRelayPorts != "" {
		lo, hi, ok := strings.Cut(c.VoiceRelayPorts, "-")
		l, err1 := strconv.ParseUint(strings.TrimSpace(lo), 10, 16)
		h, err2 := strconv.ParseUint(strings.TrimSpace(hi), 10, 16)
		if !ok || err1 != nil || err2 != nil || l == 0 || l > h {
			return VoiceSettings{}, bad("VoiceRelayPorts", "must be a range of ports, e.g. \"49152-65535\"")
		}
		v.RelayPorts = [2]uint16{uint16(l), uint16(h)}
	}
	return v, nil
}

// listenAddr parses a listen address whose host may be empty, as Go's own listeners take it. The port is
// required and may not be zero: a port picked at random is one no firewall rule names.
func listenAddr(s string) (netip.AddrPort, error) {
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		return netip.AddrPort{}, err
	}
	p, err := strconv.ParseUint(port, 10, 16)
	if err != nil || p == 0 {
		return netip.AddrPort{}, fmt.Errorf("bad port %q", port)
	}
	if host == "" {
		return netip.AddrPortFrom(netip.Addr{}, uint16(p)), nil
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return netip.AddrPort{}, err
	}
	return netip.AddrPortFrom(a.Unmap(), uint16(p)), nil
}
