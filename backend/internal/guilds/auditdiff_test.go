// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package guilds

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
	"github.com/Alexnex31/Norite/backend/internal/roles"
)

// TestSameValueComparesNumbersByValueAndEverythingElseByType pins the three helpers the diff rests on.
//
// The shape test asserts the direction that produces noise — a recorded change whose two sides are equal.
// Nothing asserted the opposite, and the opposite is the worse one: a comparison that reports two
// different values as the same suppresses a real change, leaving no entry, no error and nothing to notice.
// That is the failure this table exists for.
func TestSameValueComparesNumbersByValueAndEverythingElseByType(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		a, b any
		want bool
	}{
		// Widths, which is what asInt64 exists for: a schema column is int32 and a Go literal is int.
		{"int32 and int, equal", int32(5), 5, true},
		{"int32 and int, different", int32(5), 6, false},
		{"int64 and int32, equal", int64(64000), int32(64000), true},
		{"int16 and int64, different", int16(1), int64(2), false},

		// Named integer types compare as the numbers they are.
		{"permission and itself", roles.PermViewChannel, roles.PermViewChannel, true},
		{"permission and another", roles.PermViewChannel, roles.PermSendMessages, false},
		{"permission against its width", roles.Permission(3), 3, true},

		// Strings and bools fall through to DeepEqual.
		{"same string", "general", "general", true},
		{"different string", "general", "lobby", false},
		{"same bool", true, true, true},
		{"different bool", true, false, false},
		{"string and int are not equal", "5", 5, false},

		// Nil on either side, which every nullable column produces.
		{"nil and nil", nil, nil, true},
		{"nil and a value", nil, "general", false},
		{"a value and nil", "general", nil, false},
		{"nil and zero are not equal", nil, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, sameValue(tc.a, tc.b))
			require.Equal(t, tc.want, sameValue(tc.b, tc.a), "and the comparison is symmetric")
		})
	}
}

// TestSameValueDoesNotConflateDifferentIdentifiers is the false negative worth naming.
//
// asInt64 widens by reflect.Kind, so two *different* named integer types holding the same number compare
// equal. No call site mixes them — a field's two sides come from the same column and the same request
// field — and the alternative, comparing by type, is the mismatch that produces noise on every request.
// It is a deliberate trade rather than an oversight, so it is written down and asserted.
func TestSameValueDoesNotConflateDifferentIdentifiers(t *testing.T) {
	t.Parallel()

	require.True(t, sameValue(snowflake.ID(5), roles.Permission(5)),
		"widening by kind means two unrelated named integers holding 5 compare equal; no call site mixes "+
			"them, and comparing by type instead would record a change on every request where a column's "+
			"width differs from a literal's")
}

// TestOrNilRendersAbsenceAsNil covers the helper every nullable call site spells its two sides with.
func TestOrNilRendersAbsenceAsNil(t *testing.T) {
	t.Parallel()

	var absent *string
	require.Nil(t, orNil(absent))

	present := "general"
	require.Equal(t, "general", orNil(&present))

	// Two pointers to equal values are the same value, which is what makes changed() suppress a no-op
	// update of a nullable column.
	other := "general"
	require.True(t, sameValue(orNil(&present), orNil(&other)))
}

// TestTheDiffBuildersProduceTheDocumentedShapes is the unit-level half of the shape rule.
func TestTheDiffBuildersProduceTheDocumentedShapes(t *testing.T) {
	t.Parallel()

	d := auditDiff{}
	d.changed("name", "general", "lobby")
	d.changed("unchanged", 5, int32(5))
	d.created("topic", "rules")
	d.removed("position", int32(3))
	d.context("channel_id", snowflake.ID(42))

	require.Equal(t, map[string]any{"from": "general", "to": "lobby"}, d["name"])
	require.NotContains(t, d, "unchanged", "a field sent with the value it already had is not a change")
	require.Equal(t, map[string]any{"to": "rules"}, d["topic"])
	require.Equal(t, map[string]any{"from": int32(3)}, d["position"])
	require.Equal(t, snowflake.ID(42), d["channel_id"])

	require.NotNil(t, d.payload())
	require.Nil(t, auditDiff{}.payload(), "an empty object and null would be two spellings of nothing")
}
