// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package updater

import (
	"testing"

	"github.com/Alexnex31/Norite/backend/gatewayproto"
)

func releasesOf(t *testing.T, tags ...string) []Release {
	t.Helper()
	var out []Release
	for _, tag := range tags {
		r, ok := parseTag(tag)
		if !ok {
			t.Fatalf("%q is not a release tag", tag)
		}
		out = append(out, r)
	}
	return out
}

func version(t *testing.T, s string) gatewayproto.Version {
	t.Helper()
	v, err := gatewayproto.ParseVersion(s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestChoose(t *testing.T) {
	all := []string{"v0.1.0-alpha", "v0.1.0-rc.1", "v0.1.0", "v0.1.1", "v0.1.2-rc.1", "v0.2.0-beta.1", "v0.2.0", "v0.2.1", "v0.3.0"}
	cases := []struct {
		name        string
		current     string
		instance    string
		prereleases bool
		signedIn    bool
		bad         []string
		want        string // "" for none
	}{
		// The order rule: never to the same version, never back.
		{name: "nothing newer", current: "0.3.0", instance: "0.3.0", want: ""},
		{name: "never back, even to what the instance runs", current: "0.2.1", instance: "0.1.0", want: ""},

		// The instance rule, while MAJOR is 0: the instance's MINOR and no other.
		{name: "a patch inside the instance's minor", current: "0.1.0", instance: "0.1.0", want: "v0.1.1"},
		{name: "not past the instance's minor", current: "0.1.1", instance: "0.1.0", want: ""},
		{name: "follows an instance that moved on", current: "0.1.1", instance: "0.2.0", want: "v0.2.1"},
		{name: "a pre-release instance takes only its own version", current: "0.1.1", instance: "0.2.0-beta.1",
			prereleases: true, want: "v0.2.0-beta.1"},
		{name: "and a stable install is not offered it", current: "0.1.1", instance: "0.2.0-beta.1", want: ""},

		// Nobody to ask: the running version's own reach.
		{name: "signed out, patches only", current: "0.1.0", want: "v0.1.1"},
		{name: "signed out, a candidate becomes its release", current: "0.1.0-rc.1", want: "v0.1.1"},
		{name: "an instance with no version is nobody to ask", current: "0.1.0", instance: "dev", want: "v0.1.1"},
		{name: "nor is one whose version cannot be read", current: "0.1.0", instance: "0.1", want: "v0.1.1"},

		// Signed in, and the instance not heard from in this run: nothing, until it is. The case that
		// mattered is the first: a pre-release daemon whose pre-release instance is down was moved to the
		// release, which that instance then refused for good (M24 /code-review).
		{name: "signed in to an instance not yet heard from", current: "0.1.0-alpha", signedIn: true, want: ""},
		{name: "and not a patch either", current: "0.1.0", signedIn: true, want: ""},
		{name: "signed in to an instance that has no version to give", current: "0.1.0", signedIn: true,
			instance: "dev", want: "v0.1.1"},
		{name: "heard from, the instance decides as before", current: "0.1.0", signedIn: true, instance: "0.1.0",
			want: "v0.1.1"},

		// The channel.
		{name: "a stable install is never offered a pre-release", current: "0.1.1", instance: "0.1.0", want: ""},
		// Opted in, and still not past what the instance talks to: a release instance refuses every
		// pre-release, so the newer candidate is not taken while signed in to it.
		{name: "an opted-in one takes the highest the instance accepts", current: "0.1.0-alpha", instance: "0.1.0",
			prereleases: true, want: "v0.1.1"},
		{name: "opted in with nothing newer than a release candidate", current: "0.1.1", prereleases: true, want: "v0.1.2-rc.1"},

		// Known bad.
		{name: "a version rolled back from is not offered again", current: "0.1.0", instance: "0.1.0",
			bad: []string{"0.1.1"}, want: ""},
		{name: "and the next one still is", current: "0.2.0", instance: "0.2.0", bad: []string{"0.1.1"}, want: "v0.2.1"},
	}
	for _, tc := range cases {
		got, ok := Choose(Policy{Current: version(t, tc.current), Instance: tc.instance, SignedIn: tc.signedIn,
			Prereleases: tc.prereleases, Bad: tc.bad},
			releasesOf(t, all...))
		switch {
		case tc.want == "" && ok:
			t.Errorf("%s: chose %s, want none", tc.name, got.Tag)
		case tc.want != "" && (!ok || got.Tag != tc.want):
			t.Errorf("%s: chose %q (%v), want %s", tc.name, got.Tag, ok, tc.want)
		}
	}
}

// The listing's order is not relied on.
func TestChooseDoesNotDependOnTheListingsOrder(t *testing.T) {
	p := Policy{Current: version(t, "0.1.0"), Instance: "0.1.5"}
	for _, tags := range [][]string{{"v0.1.1", "v0.1.3", "v0.1.2"}, {"v0.1.3", "v0.1.2", "v0.1.1"}, {"v0.1.2", "v0.1.1", "v0.1.3"}} {
		if got, ok := Choose(p, releasesOf(t, tags...)); !ok || got.Tag != "v0.1.3" {
			t.Errorf("%v: chose %q", tags, got.Tag)
		}
	}
}
