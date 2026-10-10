// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package gatewayproto

import "testing"

// semver.org §11's own example, in order, with the cases M24's entry names beside it.
func TestCompareFollowsSemVerPrecedence(t *testing.T) {
	ascending := []string{
		"0.1.0-alpha", "0.1.0-rc.1", "0.1.0", "0.1.1", "0.2.0-beta.1", "0.2.0-rc.1", "0.2.0",
		"1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-alpha.beta", "1.0.0-beta", "1.0.0-beta.2", "1.0.0-beta.11",
		"1.0.0-rc.1", "1.0.0", "1.0.1", "1.1.0", "2.0.0", "10.0.0",
	}
	for i, low := range ascending {
		for j, high := range ascending {
			a, err := ParseVersion(low)
			if err != nil {
				t.Fatal(err)
			}
			b, err := ParseVersion(high)
			if err != nil {
				t.Fatal(err)
			}
			got := a.Compare(b)
			switch {
			case i < j && got >= 0, i > j && got <= 0, i == j && got != 0:
				t.Errorf("Compare(%s, %s) = %d", low, high, got)
			}
		}
	}
}

func TestCompareIgnoresBuildMetadataAndTheLeadingV(t *testing.T) {
	a, _ := ParseVersion("v1.2.3+build.5")
	b, _ := ParseVersion("1.2.3+other")
	if a.Compare(b) != 0 {
		t.Errorf("build metadata took part: %d", a.Compare(b))
	}
}

// A numeric identifier longer than any integer type still orders as a number.
func TestCompareOrdersNumbersOfAnyLength(t *testing.T) {
	small, _ := ParseVersion("1.0.0-99999999999999999999")
	large, _ := ParseVersion("1.0.0-100000000000000000000")
	if small.Compare(large) >= 0 || large.Compare(small) <= 0 {
		t.Errorf("a longer number did not sort above a shorter one")
	}
}
