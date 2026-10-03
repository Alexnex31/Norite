// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package verbs

import (
	"bufio"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/urfave/cli/v3"

	"github.com/Alexnex31/Norite/cli/internal/clierr"
	"github.com/Alexnex31/Norite/cli/internal/ops"
)

// ---------- confirming ----------

// yesFlag is the one way a script answers a confirmation.
var yesFlag = &cli.BoolFlag{Name: "yes", Aliases: []string{"y"}, Usage: "do it without asking"}

// confirm asks before something is destroyed, the one helper the destructive verbs share (M17a's open
// question).
//
// --yes answers it. On a terminal it asks, and anything but y or yes declines, which is a usage error: the
// command did nothing because it was told not to. With no terminal and no --yes it is ErrNoTerminal naming
// the flag, so a script that forgot it fails with exit 2 rather than hanging on a question nobody sees, and
// never proceeds by default.
func confirm(cmd *cli.Command, e *env, question string) error {
	if cmd.Bool("yes") {
		return nil
	}
	if !e.interactive {
		return fmt.Errorf("%w: pass --yes to %s without being asked", clierr.ErrNoTerminal, question)
	}
	// On stderr: stdout is the result, which --json pipes into a parser, and a question written there would
	// both vanish into the pipe and corrupt the document.
	if _, err := fmt.Fprintf(e.errOut, "%s? This cannot be undone. [y/N] ", upperFirst(question)); err != nil {
		return err
	}
	line, _ := bufio.NewReader(e.in).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return nil
	}
	return clierr.Usage("not confirmed; nothing was changed")
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// ---------- paging ----------

// Page is one page of a list, and the cursor for the next: what --json prints for every paged list. Next is
// null on the last page, which is a page shorter than the limit asked for — the rule every paged listing
// in openapi.yaml documents.
type Page[T any] struct {
	Items []T `json:"items"`
	// Next is the id to pass to the verb's paging flag for the following page, or null.
	Next *string `json:"next"`
}

// newPage builds a Page, its Items never null, from a page the instance returned and the limit asked for.
func newPage[T any](items []T, limit int, cursor func(T) string) Page[T] {
	if items == nil {
		items = []T{}
	}
	p := Page[T]{Items: items}
	if limit > 0 && len(items) >= limit {
		next := cursor(items[len(items)-1])
		p.Next = &next
	}
	return p
}

// limitFlag is a list's page size. Bounded here as the instance bounds it, so a value it would refuse is a
// usage error before anything is asked.
func limitFlag(def int) *cli.IntFlag {
	return &cli.IntFlag{Name: "limit", Value: def, Usage: "how many to list, 1 to 100"}
}

func beforeFlag(what string) *cli.StringFlag {
	return &cli.StringFlag{Name: "before", Usage: "list " + what + " older than this id: the `ID` the last page's next gave"}
}

func afterFlag(what string) *cli.StringFlag {
	return &cli.StringFlag{Name: "after", Usage: "list " + what + " after this `ID`"}
}

// pageOf reads a message list's paging flags into ops.Page, checked as query checks them.
func pageOf(cmd *cli.Command) (ops.Page, error) {
	p := ops.Page{Limit: int(cmd.Int("limit"))}
	if p.Limit < 1 || p.Limit > ops.MaxPage {
		return p, clierr.Usage("--limit must be between 1 and %d", ops.MaxPage)
	}
	var err error
	if p.Before, err = flagID(cmd, "before"); err != nil {
		return p, err
	}
	p.After, err = flagID(cmd, "after")
	return p, err
}

// query builds a paged list's query string from its flags. names says which of before and after the verb
// takes; extra adds the verb's own filters, already checked.
//
// A verb taking both passes both through: the instance applies them together as a window, newest first, so
// `--after X` pages by adding each page's --before and keeping --after X.
func query(cmd *cli.Command, names []string, extra url.Values) (string, int, error) {
	q := url.Values{}
	for k, v := range extra {
		q[k] = v
	}
	limit := int(cmd.Int("limit"))
	if limit < 1 || limit > 100 {
		return "", 0, clierr.Usage("--limit must be between 1 and 100")
	}
	q.Set("limit", strconv.Itoa(limit))
	for _, n := range names {
		v, err := flagID(cmd, n)
		if err != nil {
			return "", 0, err
		}
		if v != nil {
			q.Set(n, *v)
		}
	}
	return "?" + q.Encode(), limit, nil
}
