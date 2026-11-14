// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package audio

import (
	"os"
	"strconv"
	"testing"

	"github.com/jfreymuth/pulse/proto"
)

// The worker's stream is the one naming its process, and only when exactly one does: a stream that cannot
// be told apart is not judged.
func TestTheOwnStreamIsTheOneNamingThisProcess(t *testing.T) {
	me, other := strconv.Itoa(os.Getpid()), strconv.Itoa(os.Getpid()+1)
	props := func(pids ...string) []proto.PropList {
		var out []proto.PropList
		for _, p := range pids {
			l := proto.PropList{}
			if p != "" {
				l["application.process.id"] = proto.PropListString(p)
			}
			out = append(out, l)
		}
		return out
	}
	for _, c := range []struct {
		name string
		list []proto.PropList
		want int
	}{
		{"none", nil, -1},
		{"another's alone", props(other), -1},
		{"ours among others", props(other, "", me), 2},
		{"two of ours", props(me, other, me), -1},
	} {
		if got := own(len(c.list), func(i int) proto.PropList { return c.list[i] }); got != c.want {
			t.Errorf("%s: %d, want %d", c.name, got, c.want)
		}
	}
}
