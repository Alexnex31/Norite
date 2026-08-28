// Package snowflake generates the 64-bit, time-sortable identifiers every entity in Norite uses.
//
// Layout, Discord's, unchanged (ADR 0003):
//
//	 63          22          12         0
//	┌───────────┬───────────┬──────────┐
//	│ timestamp │  node ID  │ sequence │
//	│  41 bits  │  10 bits  │ 12 bits  │
//	└───────────┴───────────┴──────────┘
//
// 41 bits of milliseconds since a custom epoch covers ~69 years; 10 bits of node ID leave room for a
// multi-node deployment without an ID-scheme migration; 12 bits of sequence allow 4096 IDs per node per
// millisecond, and the generator waits for the next millisecond rather than overflowing into the node field.
//
// The high bit is left clear so every ID is a positive int64, which is what Postgres `bigint` stores and
// what Go's int64 arithmetic assumes.
//
// # Throughput
//
// 4096 IDs per millisecond per node is a hard ceiling — roughly 4.1M/second — and a caller that reaches it
// blocks until the next millisecond rather than being served a duplicate. BenchmarkNext saturates it, so
// its ~325ns/op is the rate limit rather than the cost of generating one: the assembly itself is a mutex,
// three shifts and an or, and allocates nothing. Every entity in the system shares this budget, which is
// worth remembering only if a single node ever needs to insert millions of rows a second.
//
// # Why these are not secrets
//
// A snowflake leaks its creation time to anyone who can subtract. That is intended for users, guilds, and
// messages, and it is exactly why invite codes are *not* snowflakes (ADR 0003). It also means an ID is never
// an access-control mechanism: an actor holding a valid-looking ID must still be checked for ownership on
// every request (CLAUDE.md rule 1).
package snowflake

import (
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"
)

// Epoch is the custom origin for the timestamp component: 2024-01-01T00:00:00Z, in milliseconds.
//
// A custom epoch rather than the Unix one buys back the years between 1970 and the project's existence,
// extending the usable range to roughly 2093. It is a wire-format constant — changing it would reorder
// every existing ID relative to new ones, so it never changes.
const Epoch int64 = 1704067200000

// Bit widths and derived limits.
const (
	timestampBits = 41
	nodeBits      = 10
	sequenceBits  = 12

	nodeShift      = sequenceBits
	timestampShift = sequenceBits + nodeBits

	maxNode     = -1 ^ (-1 << nodeBits)     // 1023
	maxSequence = -1 ^ (-1 << sequenceBits) // 4095

	maxTimestamp = -1 ^ (-1 << timestampBits)
)

// ErrNodeOutOfRange reports a node ID that does not fit the 10-bit field.
var ErrNodeOutOfRange = fmt.Errorf("snowflake: node ID must be between 0 and %d", maxNode)

// ErrEpochExhausted reports that the clock has moved past what 41 bits can represent. Unreachable until
// ~2093, and returned rather than ignored because silently wrapping would emit IDs that sort before every
// existing one.
var ErrEpochExhausted = errors.New("snowflake: timestamp is beyond the 41-bit range of this epoch")

// ID is a Snowflake.
//
// It is an int64 rather than a uint64 so it round-trips through Postgres `bigint` without conversion, and
// the layout keeps the sign bit clear so the value is always positive.
type ID int64

// String renders the ID as its decimal digits.
func (id ID) String() string { return strconv.FormatInt(int64(id), 10) }

// Time returns the instant the ID was generated, to millisecond precision.
func (id ID) Time() time.Time {
	ms := (int64(id) >> timestampShift) + Epoch
	return time.UnixMilli(ms).UTC()
}

// Node returns the node ID the generator was configured with.
func (id ID) Node() int64 { return (int64(id) >> nodeShift) & maxNode }

// Sequence returns the per-millisecond counter value.
func (id ID) Sequence() int64 { return int64(id) & maxSequence }

// MarshalJSON renders the ID as a *quoted string*.
//
// Not a number, and this is not cosmetic: a snowflake exceeds 2^53, so any JavaScript client parsing it as
// a JSON number silently loses precision and starts confusing adjacent IDs. Every client in this project
// therefore sees a string (docs/architecture.md §2, CLAUDE.md "IDs").
func (id ID) MarshalJSON() ([]byte, error) {
	b := make([]byte, 0, 22)
	b = append(b, '"')
	b = strconv.AppendInt(b, int64(id), 10)
	b = append(b, '"')
	return b, nil
}

// UnmarshalJSON accepts the quoted-string form, and a bare number for tolerance.
//
// Accepting the number form is deliberate leniency on input only: a hand-written curl request or an older
// client should not fail on something this cosmetic, while output stays strictly stringly-typed.
func (id *ID) UnmarshalJSON(data []byte) error {
	s := string(data)

	// The JSON literal null, before the quotes come off — never the *string* "null", which is a value
	// somebody sent. Stripping first made `{"channel_id": "null"}` indistinguishable from an absent field
	// and left the ID at whatever it already held, which for a fresh struct is zero. A silent zero in an
	// ID field is the input a permission check is least likely to notice, and the comment below already
	// gives the reason to fix this now: nothing binds an ID in a request body yet.
	if s == "null" {
		return nil
	}

	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = s[1 : len(s)-1]
	}

	// Parse rather than a bare ParseInt: the two must agree on what an ID is. They did not — a negative
	// value was refused in a URL parameter and accepted in a request body, so the same ID could be valid
	// or invalid depending only on where a handler happened to read it from. Nothing binds an ID in a
	// body yet, which is exactly why this is worth fixing now rather than after the first one does.
	parsed, err := Parse(s)
	if err != nil {
		return fmt.Errorf("snowflake: %q is not a valid ID", string(data))
	}
	*id = parsed
	return nil
}

// Parse reads an ID from its decimal string form.
//
// The single definition of "is this a valid ID", used by URL parameters and by UnmarshalJSON alike.
func Parse(s string) (ID, error) {
	parsed, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("snowflake: %q is not a valid ID", s)
	}
	// Negative values are not merely unusual: the layout packs a timestamp into the low 63 bits, so a
	// negative value has the sign bit set and cannot have come from Next. Passing one to a bigint column
	// would store a row no generator can ever produce.
	if parsed < 0 {
		return 0, fmt.Errorf("snowflake: %q is negative", s)
	}
	return ID(parsed), nil
}

// Generator produces IDs for one node.
//
// Safe for concurrent use. All generation goes through one mutex rather than atomics: the operation is a
// handful of nanoseconds, contention is bounded by how fast IDs can be consumed, and the alternative — a
// lock-free scheme over a packed timestamp-and-sequence word — is far harder to prove correct against the
// clock-regression rule below.
type Generator struct {
	node int64

	// now is the clock, overridable in tests. Time is the one input this type cannot control, and the
	// behaviors that matter most (sequence exhaustion, backwards movement) are unreachable without it.
	now func() time.Time

	mu       sync.Mutex
	lastMS   int64
	sequence int64
}

// NewGenerator builds a Generator for the given node ID.
func NewGenerator(node int64) (*Generator, error) {
	if node < 0 || node > maxNode {
		return nil, ErrNodeOutOfRange
	}
	return &Generator{node: node, now: time.Now}, nil
}

// Next returns the next ID.
//
// # Clock regression
//
// The one genuinely hard requirement (ADR 0003): NTP can step the clock backwards, and a generator that
// naively trusted it would re-emit a millisecond it has already used and produce duplicate IDs — a primary
// key collision, or worse, two entities that sort in the wrong order.
//
// This never reads the clock as authoritative. It tracks the highest millisecond it has ever issued from
// and refuses to go below it: if the clock has moved backwards, generation simply continues within that
// last millisecond, consuming sequence numbers, and waits for real time to catch up only if that
// millisecond's 4096 slots are exhausted. IDs therefore stay unique and monotonically increasing across any
// backwards step, at the cost of IDs whose embedded timestamp is slightly ahead of the wall clock until it
// catches up — the right trade, since a duplicate ID is unrecoverable and a few milliseconds of timestamp
// skew is not.
func (g *Generator) Next() (ID, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	ms := g.nowMS()

	switch {
	case ms > g.lastMS:
		// Ordinary case: time has advanced, start a fresh sequence.
		g.lastMS = ms
		g.sequence = 0

	default:
		// Same millisecond, or the clock went backwards. Both are handled identically — keep issuing from
		// the last millisecond we used — which is what makes a backwards step harmless rather than fatal.
		g.sequence++
		if g.sequence > maxSequence {
			// 4096 IDs used inside one millisecond. Wait for the clock to pass it rather than overflowing
			// the sequence into the node-ID field, which would forge another node's identity.
			g.lastMS = g.waitPast(g.lastMS)
			g.sequence = 0
		}
	}

	if g.lastMS > maxTimestamp {
		return 0, ErrEpochExhausted
	}

	return ID(g.lastMS<<timestampShift | g.node<<nodeShift | g.sequence), nil
}

// MustNext is Next without the error, for call sites where an error is impossible or fatal anyway.
//
// The only failure Next can report is epoch exhaustion, which is a ~2093 problem and not something a caller
// in a request path can do anything about.
func (g *Generator) MustNext() ID {
	id, err := g.Next()
	if err != nil {
		panic(err)
	}
	return id
}

// nowMS returns the current time as milliseconds since Epoch.
func (g *Generator) nowMS() int64 { return g.now().UnixMilli() - Epoch }

// waitPast blocks until the clock reads later than ms, and returns the new value.
//
// Only reachable on sequence exhaustion (4096 IDs in one millisecond) or while the clock is behind. The
// sleep is deliberately coarse — sub-millisecond precision buys nothing when waiting for a millisecond
// boundary, and a tight spin would burn a core.
func (g *Generator) waitPast(ms int64) int64 {
	for {
		current := g.nowMS()
		if current > ms {
			return current
		}
		time.Sleep(200 * time.Microsecond)
	}
}
