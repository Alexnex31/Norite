// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

// Command voice-sfu hosts the SFU and TURN for a manual pass, and for M29 before M33 exists: the stack
// norite-server starts, and a plain HTTP endpoint that hands out TURN credentials and provisions a
// connection for whoever posts an offer.
//
// It is a test harness and never a release: nothing authenticates the endpoint, which is the part M33
// builds, so it binds loopback unless told otherwise, and only to a network the tester owns. It calls
// the same StartStack, Mint and Provision the server and the tests do, so M33 removes nothing when it
// arrives.
//
//	go run ./internal/voice/voicetest/cmd/voice-sfu -media 10.9.0.1:3479 -turn 10.9.0.1:3478 \
//		-http 10.9.0.1:8089
package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rs/zerolog"

	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
	"github.com/Alexnex31/Norite/backend/internal/voice"
	"github.com/Alexnex31/Norite/backend/internal/voice/turn"
	"github.com/Alexnex31/Norite/backend/internal/voice/voicetest"
)

func main() {
	media := flag.String("media", "127.0.0.1:3479", "the UDP address every connection's media arrives on")
	httpAddr := flag.String("http", "127.0.0.1:8089", "the address the provisioning endpoint listens on")
	loopback := flag.Bool("loopback", false, "offer loopback candidates, for clients on this machine "+
		"(always, when -media is a loopback address)")
	turnAddr := flag.String("turn", "127.0.0.1:3478", "TURN's address, on UDP and TCP; its host is the one "+
		"clients are told, so name an address they reach")
	turnTLS := flag.String("turn-tls", "", "TURN's TLS address, with -tls-cert and -tls-key")
	cert := flag.String("tls-cert", "", "the PEM certificate TURN serves over TLS")
	key := flag.String("tls-key", "", "its key")
	flag.Parse()
	// Pion's ICE drops loopback addresses unless asked, so an SFU on 127.0.0.1 without this offers no
	// candidate at all and nothing can connect.
	if host, _, err := net.SplitHostPort(*media); err == nil {
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
			*loopback = true
		}
	}
	if err := run(*media, *httpAddr, *loopback, *turnAddr, *turnTLS, *cert, *key); err != nil {
		fmt.Fprintln(os.Stderr, "voice-sfu:", err)
		os.Exit(1)
	}
}

func run(media, httpAddr string, loopback bool, turnAddr, turnTLS, cert, key string) error {
	log := zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr}).With().Timestamp().Logger().Level(zerolog.DebugLevel)
	mediaAddr, err := netip.ParseAddrPort(media)
	if err != nil || mediaAddr.Addr().IsUnspecified() {
		return fmt.Errorf("-media names one address and a port, which TURN's relay reaches the SFU at")
	}
	// A fresh key per run: the harness has no instance, and a credential outliving the process it was
	// minted by has nothing to reach.
	signingKey := make([]byte, 32)
	_, _ = rand.Read(signingKey)
	stack, err := voice.StartStack(voice.StackOptions{
		MediaListen:   media,
		TURNListen:    turnAddr,
		TURNTLSListen: turnTLS,
		TLSCert:       cert,
		TLSKey:        key,
		Relay:         mediaAddr.Addr(),
		SFU:           mediaAddr,
		SigningKey:    signingKey,
		Loopback:      loopback,
		Version:       "dev",
		Logger:        log,
	})
	if err != nil {
		return err
	}
	defer func() { _ = stack.Close() }()
	sfu := stack.SFU
	urls := []string{"turn:" + turnAddr + "?transport=udp", "turn:" + turnAddr + "?transport=tcp"}
	if turnTLS != "" {
		urls = append(urls, "turns:"+turnTLS+"?transport=tcp")
	}

	ids, err := snowflake.NewGenerator(1)
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /credentials", func(w http.ResponseWriter, r *http.Request) {
		var req voicetest.CredentialsRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
			http.Error(w, "a credentials request is JSON: user", http.StatusBadRequest)
			return
		}
		user, err := snowflake.Parse(req.User)
		if err != nil {
			http.Error(w, "user is an id", http.StatusBadRequest)
			return
		}
		c := turn.Mint(stack.Secret, user, time.Now())
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(voicetest.CredentialsResponse{Username: c.Username, Password: c.Password, URLs: urls})
	})
	mux.HandleFunc("POST /provision", func(w http.ResponseWriter, r *http.Request) {
		var req voicetest.ProvisionRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
			http.Error(w, "a provision request is JSON: room, user, offer", http.StatusBadRequest)
			return
		}
		room, err1 := snowflake.Parse(req.Room)
		user, err2 := snowflake.Parse(req.User)
		if err := errors.Join(err1, err2); err != nil {
			http.Error(w, "room and user are ids", http.StatusBadRequest)
			return
		}
		id := ids.MustNext()
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		answer, err := sfu.Provision(ctx, room, user, id, ids.MustNext(), req.Offer)
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		log.Info().Stringer("room", room).Stringer("user", user).Stringer("conn", id).Msg("provisioned")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(voicetest.ProvisionResponse{Conn: id.String(), Answer: answer})
	})
	mux.HandleFunc("POST /evict/{conn}", func(w http.ResponseWriter, r *http.Request) {
		id, err := snowflake.Parse(r.PathValue("conn"))
		if err != nil {
			http.Error(w, "not a connection id", http.StatusBadRequest)
			return
		}
		_ = sfu.EvictConnection(r.Context(), id)
		w.WriteHeader(http.StatusNoContent)
	})

	srv := &http.Server{Addr: httpAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info().Str("media", media).Strs("turn", urls).Str("http", httpAddr).Msg("voice-sfu up")

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	select {
	case err := <-errc:
		return err
	case <-stop:
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(ctx)
}
