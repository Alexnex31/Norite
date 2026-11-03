// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

// Command voice-client is one voice connection for a manual pass: it posts its offer to a cmd/voice-sfu,
// connects, sends the committed tone for as long as asked, and writes what each slot received as an Ogg
// file a person can play. A test harness, never a release.
//
//	go run ./internal/voice/voicetest/cmd/voice-client -sfu http://10.9.0.1:8089 -room 1 -user 2 -out b
//
// With -relay tcp, the client offers nothing but a candidate relayed by the harness's TURN over that
// transport, as a client whose network refuses UDP to the SFU ends up doing. A TURNS server is verified
// against the system's roots, so a test certificate goes in SSL_CERT_FILE.
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/Alexnex31/Norite/backend/internal/voice/voicetest"
)

func main() {
	sfu := flag.String("sfu", "http://127.0.0.1:8089", "the harness's provisioning endpoint")
	room := flag.String("room", "1", "the room's id")
	user := flag.String("user", "", "this client's user id")
	slots := flag.Int("slots", 2, "how many slots to receive on")
	seconds := flag.Int("seconds", 10, "how long to send and receive")
	out := flag.String("out", "received", "the prefix of the Ogg files written, one per slot")
	loopback := flag.Bool("loopback", false, "offer loopback candidates, for an SFU on this machine "+
		"(always, when -sfu names a loopback address)")
	relay := flag.String("relay", "", "join through TURN alone, over udp, tcp or tls")
	flag.Parse()
	if *relay != "" && *relay != "udp" && *relay != "tcp" && *relay != "tls" {
		fmt.Fprintln(os.Stderr, "voice-client: -relay is udp, tcp or tls")
		os.Exit(2)
	}
	if u, err := url.Parse(*sfu); err == nil {
		if ip := net.ParseIP(u.Hostname()); (ip != nil && ip.IsLoopback()) || u.Hostname() == "localhost" {
			*loopback = true
		}
	}
	if *user == "" {
		fmt.Fprintln(os.Stderr, "voice-client: -user is required")
		os.Exit(2)
	}
	if err := run(*sfu, *room, *user, *slots, time.Duration(*seconds)*time.Second, *out, *loopback, *relay); err != nil {
		fmt.Fprintln(os.Stderr, "voice-client:", err)
		os.Exit(1)
	}
}

func run(sfu, room, user string, slots int, d time.Duration, out string, loopback bool, relay string) error {
	ctx, cancel := context.WithTimeout(context.Background(), d+time.Minute)
	defer cancel()

	opts := voicetest.Options{Slots: slots, Loopback: loopback}
	if relay != "" {
		cred, err := voicetest.Credentials(ctx, sfu, voicetest.CredentialsRequest{User: user})
		if err != nil {
			return err
		}
		scheme := map[string]string{"udp": "turn:", "tcp": "turn:", "tls": "turns:"}[relay]
		transport := map[string]string{"udp": "udp", "tcp": "tcp", "tls": "tcp"}[relay]
		for _, u := range cred.URLs {
			if strings.HasPrefix(u, scheme) && strings.HasSuffix(u, "?transport="+transport) {
				opts.ICEServers = []webrtc.ICEServer{{URLs: []string{u}, Username: cred.Username, Credential: cred.Password}}
			}
		}
		if opts.ICEServers == nil {
			return fmt.Errorf("the harness offers no TURN over %s: %v", relay, cred.URLs)
		}
		opts.RelayOnly = true
	}
	c, err := voicetest.New(opts)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()

	offer, err := c.Offer(ctx)
	if err != nil {
		return err
	}
	res, err := voicetest.Provision(ctx, sfu, voicetest.ProvisionRequest{Room: room, User: user, Offer: offer})
	if err != nil {
		return err
	}
	if err := c.Accept(res.Answer); err != nil {
		return err
	}
	if err := c.WaitConnected(ctx); err != nil {
		return err
	}
	fmt.Printf("connected as connection %s through a %s candidate, %d bytes of offer\n", res.Conn,
		c.SelectedCandidateType(), len(offer))

	tone := voicetest.TonePackets()
	var packets [][]byte
	for len(packets)*20 < int(d.Milliseconds()) {
		packets = append(packets, tone...)
	}
	packets = packets[:int(d.Milliseconds())/20]
	if err := c.Send(ctx, packets, voicetest.SendOptions{Level: voicetest.Loud}); err != nil {
		return err
	}
	// Long enough for another client that started a moment later to finish too.
	time.Sleep(2 * time.Second)

	if m, ok := c.LatestSlots(); ok {
		for _, e := range m.Map {
			fmt.Printf("slot %d: user %s, connection %s, source %d\n", e.Slot, e.User, e.Conn, e.Source)
		}
	}
	for i, mid := range c.SlotMids() {
		got := c.Received(mid)
		sources := map[uint32]int{}
		for _, p := range got {
			if len(p.CSRC) > 0 {
				sources[p.CSRC[0]]++
			}
		}
		name := fmt.Sprintf("%s-slot%d.ogg", out, i)
		f, err := os.Create(name)
		if err != nil {
			return err
		}
		if err := voicetest.WriteOgg(f, got); err != nil {
			return err
		}
		fmt.Printf("slot %d: %d packets from sources %v, written to %s\n", i, len(got), sources, name)
	}
	for _, m := range c.Messages() {
		fmt.Printf("received: %s\n", m)
	}
	return nil
}
