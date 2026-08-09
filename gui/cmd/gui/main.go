// Command gui is the Norite native GUI.
//
// M0 scope: proves the module builds. The real Gio app scaffold, attaching to the daemon over the local
// socket, is Milestone M70 (Phase J) — see docs/architecture.md §13 and ADR 0009.
package main

import "fmt"

func main() {
	fmt.Println("norite gui (M0 skeleton) — no window wired up yet")
}
