// Command server is the Norite backend composition root.
//
// M0 scope: boots an empty process with no routes, no database connection, and no business logic — just
// enough to prove the module/build/CI plumbing works end to end. The chi router, pgxpool wiring, and
// /healthz endpoint are M1 scope (see docs/architecture.md §13).
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	addr := os.Getenv("NORITE_LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	srv := &http.Server{Addr: addr}

	go func() {
		log.Printf("norite backend (M0 skeleton) listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Fatalf("graceful shutdown failed: %v", err)
	}
	log.Println("norite backend stopped")
}
