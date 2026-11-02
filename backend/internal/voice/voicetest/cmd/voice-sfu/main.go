// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

// Command voice-sfu hosts the SFU for a manual pass, and for M27 and M29 before M33 exists: the SFU on one
// UDP port, and a plain HTTP endpoint that provisions a connection for whoever posts an offer.
//
// It is a test harness and never a release: nothing authenticates the endpoint, which is the part M33
// builds, so it binds loopback unless told otherwise, and only to a network the tester owns. It calls
// the same Provision the tests call, so M33 removes nothing when it arrives.
//
//	go run ./internal/voice/voicetest/cmd/voice-sfu -media 10.9.0.1:3479 -http 10.9.0.1:8089
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rs/zerolog"

	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
	"github.com/Alexnex31/Norite/backend/internal/voice"
	"github.com/Alexnex31/Norite/backend/internal/voice/voicetest"
)

func main() {
	media := flag.String("media", "127.0.0.1:3479", "the UDP address every connection's media arrives on")
	httpAddr := flag.String("http", "127.0.0.1:8089", "the address the provisioning endpoint listens on")
	loopback := flag.Bool("loopback", false, "offer loopback candidates, for clients on this machine "+
		"(always, when -media is a loopback address)")
	flag.Parse()
	// Pion's ICE drops loopback addresses unless asked, so an SFU on 127.0.0.1 without this offers no
	// candidate at all and nothing can connect.
	if host, _, err := net.SplitHostPort(*media); err == nil {
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
			*loopback = true
		}
	}
	if err := run(*media, *httpAddr, *loopback); err != nil {
		fmt.Fprintln(os.Stderr, "voice-sfu:", err)
		os.Exit(1)
	}
}

func run(media, httpAddr string, loopback bool) error {
	log := zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr}).With().Timestamp().Logger().Level(zerolog.DebugLevel)
	conn, err := net.ListenPacket("udp", media)
	if err != nil {
		return err
	}
	sfu, err := voice.New(voice.Options{
		Conn: conn, Loopback: loopback, Version: "dev", Limits: voice.DefaultLimits(), Logger: log,
	})
	if err != nil {
		return err
	}
	defer func() { _ = sfu.Close() }()

	ids, err := snowflake.NewGenerator(1)
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
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
	log.Info().Str("media", conn.LocalAddr().String()).Str("http", httpAddr).Msg("voice-sfu up")

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
