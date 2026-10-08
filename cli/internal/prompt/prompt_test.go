// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package prompt

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/Alexnex31/Norite/cli/internal/clierr"
)

func ask(yes, interactive bool, typed string) (asked string, err error) {
	var out bytes.Buffer
	err = Confirm{
		Yes: yes, Interactive: interactive, In: strings.NewReader(typed), Out: &out,
		Question: "Delete it?", Otherwise: "pass --yes to delete it without being asked",
	}.Ask()
	return out.String(), err
}

// --yes answers the question, so it is not asked, terminal or no.
func TestYesAnswersWithoutAsking(t *testing.T) {
	for _, interactive := range []bool{true, false} {
		asked, err := ask(true, interactive, "")
		if err != nil || asked != "" {
			t.Errorf("interactive=%v: asked %q, err %v", interactive, asked, err)
		}
	}
}

// With nobody to ask and no --yes, nothing is done and the error names what to pass: exit 2, never a hang
// and never a default of yes.
func TestWithNoTerminalItRefusesAndNamesTheFlag(t *testing.T) {
	asked, err := ask(false, false, "y\n")
	if !errors.Is(err, clierr.ErrNoTerminal) {
		t.Fatalf("err = %v, want ErrNoTerminal", err)
	}
	if !strings.Contains(err.Error(), "pass --yes to delete it") {
		t.Errorf("the error does not say what to pass: %v", err)
	}
	if asked != "" {
		t.Errorf("a question was written to nobody: %q", asked)
	}
}

// Only y and yes are yes. Anything else, an empty line and a closed stdin included, declines, and declining
// is a usage error: the command did nothing because it was told not to.
func TestOnlyYAndYesAreYes(t *testing.T) {
	for _, typed := range []string{"y\n", "Y\n", "yes\n", "  YES  \n", "y"} {
		asked, err := ask(false, true, typed)
		if err != nil {
			t.Errorf("%q: %v", typed, err)
		}
		if asked != "Delete it? [y/N] " {
			t.Errorf("%q: asked %q", typed, asked)
		}
	}
	for _, typed := range []string{"", "\n", "n\n", "no\n", "yep\n", "yy\n", "ye\n"} {
		_, err := ask(false, true, typed)
		var usage *clierr.UsageError
		if !errors.As(err, &usage) {
			t.Errorf("%q: err = %v, want a usage error", typed, err)
		}
	}
}

func TestAsksSaysWhetherAQuestionWillBePut(t *testing.T) {
	for _, tc := range []struct{ yes, interactive, want bool }{
		{false, true, true}, {true, true, false}, {false, false, false}, {true, false, false},
	} {
		if got := (Confirm{Yes: tc.yes, Interactive: tc.interactive}).Asks(); got != tc.want {
			t.Errorf("yes=%v interactive=%v: Asks() = %v", tc.yes, tc.interactive, got)
		}
	}
}
