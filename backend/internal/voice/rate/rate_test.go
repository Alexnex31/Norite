// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package rate

import (
	"testing"
	"time"
)

func TestMeterAdmitsTheBoundAndStrikesPastTwiceIt(t *testing.T) {
	var m Meter
	at := time.Now()
	burst := func(n int) (admitted int, out bool) {
		for i := range n {
			ok, o := m.Admit(at.Add(time.Duration(i)*time.Second/time.Duration(n)), 0, 10, 0, 2)
			if ok {
				admitted++
			}
			out = out || o
		}
		at = at.Add(time.Second)
		return admitted, out
	}
	if got, out := burst(15); got != 10 || out {
		t.Fatalf("one second at 1.5x the bound: %d admitted, out %v", got, out)
	}
	if _, out := burst(15); out {
		t.Fatal("a second at 1.5x struck out")
	}
	burst(25)
	if _, out := burst(25); out {
		t.Fatal("one strike is out")
	}
	if _, out := burst(1); !out {
		t.Fatal("two seconds past twice the bound are not out")
	}

	var clean Meter
	at = time.Now()
	for i := range 25 {
		clean.Admit(at.Add(time.Duration(i)*40*time.Millisecond), 0, 10, 0, 2)
	}
	clean.Admit(at.Add(5*time.Second), 0, 10, 0, 2)
	for i := range 25 {
		if _, out := clean.Admit(at.Add(5*time.Second+time.Duration(i)*40*time.Millisecond), 0, 10, 0, 2); out {
			t.Fatal("a strike survived a quiet gap")
		}
	}

	var bits Meter
	if ok, _ := bits.Admit(time.Now(), 200, 100, 1000, 3); ok {
		t.Fatal("1600 bits admitted against a bound of 1000")
	}
}
