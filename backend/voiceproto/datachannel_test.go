// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package voiceproto

import (
	"errors"
	"testing"
)

func TestDecodeRefusesAnythingAfterTheValue(t *testing.T) {
	for _, in := range []string{
		`{"op":"hello","version":"dev"}]anything`,
		`{"op":"hello","version":"dev"}}garbage`,
		`{"op":"hello","version":"dev"} {}`,
		`{"op":"hello","version":"dev"}x`,
	} {
		var h Hello
		if err := Decode([]byte(in), &h); !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: got %v, want ErrMalformed", in, err)
		}
	}
	var h Hello
	if err := Decode([]byte("  {\"op\":\"hello\",\"version\":\"dev\"}\n "), &h); err != nil {
		t.Fatalf("a value with surrounding space: %v", err)
	}
}
