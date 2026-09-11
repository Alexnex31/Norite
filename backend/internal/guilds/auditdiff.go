// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package guilds

import "reflect"

// auditDiff is one entry's `changes` payload, built field by field.
//
// # The shape, and the rule that makes it readable
//
// Two kinds of key, and the difference is visible in the value rather than declared anywhere:
//
//   - a **changed field** is an object carrying `from`, `to`, or both —
//     `{"name": {"from": "general", "to": "lobby"}}`;
//   - a **context field** is a scalar — `{"channel_id": "42"}`.
//
// Context exists because `audit_log_entries.target_id` is one column and some actions act on a pair: an
// overwrite names a channel *and* a target, a role grant names a member *and* a role. The column holds
// one of them and the payload carries the other. It is not a change, and rendering it as `{"to": …}`
// would say the channel was set to that value.
//
// So a renderer can walk the map and ask one question per key. [TestTheAuditDiffShapeIsUniform] asserts
// it holds for every action this package writes, because a rule stated only in a comment is one the
// seventeenth mutation breaks.
//
// # Why the diff is built here and not in the reader
//
// A diff needs the prior state, and the prior state is the row being written over — so it is available
// exactly once, inside the transaction that replaces it. The reader cannot reconstruct `from`: the
// previous entry for that object may have touched different fields, or may not exist at all because the
// row predates the log. That is the whole reason this touches every mutation rather than one function.
//
// # Values carry their wire type
//
// A permission goes in as a [roles.Permission] and a snowflake as a [snowflake.ID], never as an int64 —
// both marshal themselves as quoted decimal strings, which is what the contract says those types are. M12
// and M13 wrote `in.Permissions.Int64()` here, which rendered a bitfield as a JSON number and put the
// float64 hazard the string representation exists to avoid straight back into the payload. Twenty bits
// are defined, so no value is near 2^53 and nothing is broken today — which is precisely when it is free
// to fix, the argument M12 made for the representation in the first place.
type auditDiff map[string]any

// changed records a field's transition, and only when there is one.
//
// A field sent with the value it already had is not a change, and recording it would make an audit log
// answer "what did this request contain" rather than "what happened" — the two differ on exactly the
// request a client fires on every keystroke of an edit form. Both sides are kept even when one is null,
// since "the topic was removed" needs the topic it had.
func (d auditDiff) changed(field string, from, to any) {
	if reflect.DeepEqual(from, to) {
		return
	}
	d[field] = map[string]any{"from": from, "to": to}
}

// created records a field a creation set.
//
// No `from`, because there was no prior state. A `"from": null` on every field of every create would be
// noise that a reader has to skip past on the most common entry in the log.
func (d auditDiff) created(field string, to any) {
	d[field] = map[string]any{"to": to}
}

// removed records a field a deletion destroyed.
//
// No `to`, symmetrically — and this is the half that carries the weight. A deletion entry whose target no
// longer exists is the one case where the log is the only surviving description of the object, which is
// why `*.delete` records the fields that identify what went rather than nothing at all.
func (d auditDiff) removed(field string, from any) {
	d[field] = map[string]any{"from": from}
}

// context records an identifying value that is not a change.
//
// Takes a scalar by contract rather than by signature — Go has no type for "not a map" — and the shape
// test is what enforces it. Passing an object here would make the value indistinguishable from a diff.
func (d auditDiff) context(field string, value any) {
	d[field] = value
}

// orNil renders a nullable pointer as the value or an explicit nil, so a diff's two sides compare and
// marshal the way the column does.
//
// Without it a *string is compared as a pointer identity by the caller's eye and as a dereferenced value
// by reflect.DeepEqual, which happen to agree — and a nil *string marshals as null, which is right. It
// exists to make that agreement deliberate rather than incidental, and to give the call sites one spelling.
func orNil[T any](v *T) any {
	if v == nil {
		return nil
	}
	return *v
}

// payload returns the map to store, or nil when nothing was recorded.
//
// writeAudit leaves the column NULL for an empty map, and an empty object would be a third state meaning
// the same thing. A mutation that changed nothing still writes its entry — the action happened — with no
// changes attached.
func (d auditDiff) payload() map[string]any {
	if len(d) == 0 {
		return nil
	}
	return d
}
