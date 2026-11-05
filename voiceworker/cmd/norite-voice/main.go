// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

// Command norite-voice is the voice-worker: the process the daemon starts when the user joins a voice
// channel, and which owns that call's media (rule 11). The daemon starts it with two pipes and nothing
// else to say; a person never runs it, and it says so when one does.
//
// It reads its pipes' numbers from -pipes (daemon/voiceipc), never stdin and stdout: what it prints, Pion's
// lines and from M29 whatever a C library writes, goes to the daemon's log and cannot land in a frame.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/Alexnex31/Norite/daemon/voiceipc"
	"github.com/Alexnex31/Norite/voiceworker/internal/audio"
	"github.com/Alexnex31/Norite/voiceworker/internal/notices"
	"github.com/Alexnex31/Norite/voiceworker/internal/session"
	"github.com/Alexnex31/Norite/voiceworker/internal/worker"
)

// Version is the build's version string, overridden at link time by goreleaser, as the daemon's is.
var Version = "dev"

// exitUsage is a worker started by hand, the clients' code for a usage error.
const exitUsage = 2

// closeGrace is how long a call is given to close once the daemon has gone, before the worker exits
// anyway: a library stuck inside it must not keep a worker alive past its daemon.
const closeGrace = 2 * time.Second

func main() {
	showVersion := flag.Bool("version", false, "print the version and exit")
	showLicense := flag.Bool("licenses", false, "print the third-party licenses in this binary and exit")
	pipes := flag.String(voiceipc.PipesFlag, "", "the daemon's two pipes; set by the daemon, never by hand")
	flag.Parse()

	if *showVersion {
		fmt.Println(Version)
		return
	}
	// The worker ships beside the daemon and carries its own attribution, as each binary does, since it
	// links Pion and from M29 C libraries the daemon does not.
	if *showLicense {
		fmt.Print(notices.Text)
		return
	}
	if *pipes == "" || flag.NArg() > 0 {
		fmt.Fprintln(os.Stderr, "norite-voice is started by norite-daemon when you join a voice call, "+
			"and is not run by hand.")
		os.Exit(exitUsage)
	}
	read, write, err := voiceipc.ParsePipes(*pipes)
	if err != nil {
		fmt.Fprintf(os.Stderr, "norite-voice: %v\n", err)
		os.Exit(exitUsage)
	}
	in, out := os.NewFile(read, "control"), os.NewFile(write, "events")
	if in == nil || out == nil {
		fmt.Fprintln(os.Stderr, "norite-voice: the pipes it was given are not open")
		os.Exit(exitUsage)
	}

	code := worker.Run(context.Background(), in, out, worker.Options{
		Version: Version,
		Close:   closeGrace,
		Dial: func(join voiceipc.Join) (worker.Call, error) {
			return session.New(join, session.Options{Version: Version, Log: os.Stderr, Device: audio.System()})
		},
		// The daemon has gone, by a leave or by its own end. Whatever the loop is doing, the worker is gone
		// shortly after: the loop closes the call and returns first when it can.
		Gone: func(error) { time.AfterFunc(closeGrace+time.Second, func() { os.Exit(worker.ExitOK) }) },
	})
	os.Exit(code)
}
