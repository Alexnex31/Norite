// Command daemond is the shared Norite background daemon.
//
// M0 scope: proves the module builds and can start/stop cleanly, standing in for the "installs itself as an
// OS-level service but does nothing beyond starting and stopping cleanly" requirement of Milestone M3 (see
// docs/roadmap.md and ADR 0010). Real OS-service auto-install, the gateway client, and dual IPC are
// later milestones (M3, M18-M24).
package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	log.Println("norite daemon (M0 skeleton) starting")

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	log.Println("norite daemon stopped")
}
