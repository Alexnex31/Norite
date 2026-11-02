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

// encoding/json takes a key in any case, a missing field as its zero value and null as an empty array. The
// contract takes none of them, and a client written against a lenient server would rely on all three.
func TestDecodeHoldsTheContractsShape(t *testing.T) {
	refused := []struct {
		in string
		v  any
	}{
		{`{"OP":"subscribe","PIN":null,"drop":[]}`, &Subscribe{}},
		{`{"op":"subscribe","pin":null,"drop":[]}`, &Subscribe{}},
		{`{"op":"subscribe","pin":[null],"drop":[]}`, &Subscribe{}},
		{`{"op":"subscribe","drop":[]}`, &Subscribe{}},
		{`{"op":"subscribe","pin":[],"drop":[],"cap":null}`, &Subscribe{}},
		{`{"op":"hello"}`, &Hello{}},
		{`{"op":"hello","Version":"dev"}`, &Hello{}},
		{`{"op":"hello","version":"dev","Version":"dev"}`, &Hello{}},
		{`null`, &Hello{}},
		{`{"op":"slots","map":[{"slot":0,"kind":"audio","source":1,"user":"1"}]}`, &Slots{}},
		{`{"op":"slots","map":[{"Slot":0,"kind":"audio","source":1,"user":"1","conn":"2"}]}`, &Slots{}},
	}
	for _, tc := range refused {
		if err := Decode([]byte(tc.in), tc.v); !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: got %v, want ErrMalformed", tc.in, err)
		}
	}
	for _, tc := range []struct {
		in string
		v  any
	}{
		{`{"op":"subscribe","pin":[],"drop":[]}`, &Subscribe{}},
		{`{"op":"subscribe","pin":["1"],"drop":[],"cap":0}`, &Subscribe{}},
		{`{"op":"slots","map":[{"slot":0,"kind":"audio","source":1,"user":"1","conn":"2"}]}`, &Slots{}},
	} {
		if err := Decode([]byte(tc.in), tc.v); err != nil {
			t.Errorf("%s: %v", tc.in, err)
		}
	}
}
