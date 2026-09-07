// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package cliapp

import (
	"context"
	"fmt"

	"github.com/urfave/cli/v3"

	"github.com/Alexnex31/Norite/cli/internal/notices"
)

// licensesCommand prints the third-party notices compiled into this binary.
//
// A subcommand rather than a global flag, because it emits a document rather than a value: `--version`
// answers a question about this build in one line, and this answers an obligation in tens of kilobytes.
// A flag on the root command would also have to be honored by every subcommand to be discoverable, which
// is a lot of surface for something read once.
//
// Deliberately not sanitized through termsafe (rule 19). Every byte here was compiled in from a file this
// repository generates and commits, so it is not foreign text arriving at runtime — and a license text is
// exactly the kind of document where replacing an unusual character with U+FFFD would corrupt the thing
// the command exists to reproduce faithfully.
func licensesCommand() *cli.Command {
	return &cli.Command{
		Name:  "licenses",
		Usage: "Print the licenses of the third-party code in this binary",
		Description: "Norite itself is free software under the AGPL-3.0-or-later; `norite --version` and\n" +
			"the LICENSE file at the repository root cover that. This command prints the separate\n" +
			"obligation: the full license text of every third-party module linked into this binary,\n" +
			"which MIT, BSD and Apache-2.0 each require to accompany it.\n\n" +
			"The daemon and the server are separate binaries with their own dependency sets, so this\n" +
			"is not the whole project's answer — only this one executable's.",
		Action: func(_ context.Context, cmd *cli.Command) error {
			// Blank assignment is this codebase's "deliberately ignored" (errcheck check-blank: false):
			// a write failure on stdout is the shell's business, and there is nowhere to report it to.
			_, _ = fmt.Fprint(cmd.Writer, notices.Text)
			return nil
		},
	}
}
