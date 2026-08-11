package snowflake

import (
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeClock is a manually-advanced clock. Time is the one input a Generator cannot control, and the two
// behaviors that matter most — sequence exhaustion and backwards movement — are unreachable without it.
type fakeClock struct {
	mu sync.Mutex
	ms int64
}

func newFakeClock(msSinceEpoch int64) *fakeClock { return &fakeClock{ms: msSinceEpoch} }

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.UnixMilli(Epoch + c.ms).UTC()
}

func (c *fakeClock) advance(ms int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ms += ms
}

func (c *fakeClock) set(ms int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ms = ms
}

// generatorAt builds a Generator driven by a fake clock.
func generatorAt(t *testing.T, node, ms int64) (*Generator, *fakeClock) {
	t.Helper()
	g, err := NewGenerator(node)
	require.NoError(t, err)
	clock := newFakeClock(ms)
	g.now = clock.now
	return g, clock
}

func TestNewGeneratorRejectsAnOutOfRangeNode(t *testing.T) {
	for _, node := range []int64{-1, maxNode + 1, 5000} {
		_, err := NewGenerator(node)
		assert.ErrorIs(t, err, ErrNodeOutOfRange, "node %d must be rejected", node)
	}
	for _, node := range []int64{0, 1, maxNode} {
		_, err := NewGenerator(node)
		assert.NoError(t, err, "node %d must be accepted", node)
	}
}

func TestIDsAreUniqueAndIncreasing(t *testing.T) {
	g, clock := generatorAt(t, 1, 1000)

	seen := make(map[ID]struct{}, 10000)
	var previous ID
	for i := range 10000 {
		id, err := g.Next()
		require.NoError(t, err)

		_, duplicate := seen[id]
		require.False(t, duplicate, "duplicate ID at iteration %d: %s", i, id)
		seen[id] = struct{}{}

		require.Greater(t, id, previous, "IDs must strictly increase")
		previous = id

		if i%1000 == 0 {
			clock.advance(1)
		}
	}
}

// The single most important property in this package (ADR 0003). NTP can step the clock backwards; a
// generator that trusted it would re-issue a millisecond it had already used and produce duplicate primary
// keys, or entities that sort in the wrong order.
func TestIDsStayUniqueAndIncreasingAcrossABackwardsClockStep(t *testing.T) {
	g, clock := generatorAt(t, 1, 100000)

	var ids []ID
	for range 50 {
		id, err := g.Next()
		require.NoError(t, err)
		ids = append(ids, id)
	}

	// A large NTP correction backwards — far bigger than anything realistic, so the property is tested well
	// past the boundary rather than at it.
	clock.set(90000)

	for range 50 {
		id, err := g.Next()
		require.NoError(t, err)
		ids = append(ids, id)
	}

	// And forwards again, past the original point, to confirm the generator resumes tracking real time
	// rather than staying stuck in its own.
	clock.set(200000)
	for range 50 {
		id, err := g.Next()
		require.NoError(t, err)
		ids = append(ids, id)
	}

	seen := make(map[ID]struct{}, len(ids))
	for i, id := range ids {
		_, duplicate := seen[id]
		require.False(t, duplicate, "duplicate ID at index %d after the clock moved backwards", i)
		seen[id] = struct{}{}
		if i > 0 {
			require.Greater(t, id, ids[i-1], "ID at index %d went backwards", i)
		}
	}
}

// 4096 IDs fit in one millisecond. The 4097th must wait for the clock rather than overflow the sequence
// into the node-ID field, which would forge another node's identity and collide with its IDs.
func TestSequenceExhaustionWaitsInsteadOfOverflowingIntoTheNodeField(t *testing.T) {
	g, clock := generatorAt(t, 7, 5000)

	// Drain exactly one millisecond's worth.
	var last ID
	for i := range maxSequence + 1 {
		id, err := g.Next()
		require.NoError(t, err)
		require.EqualValues(t, 7, id.Node(), "node ID changed at iteration %d", i)
		last = id
	}
	require.EqualValues(t, maxSequence, last.Sequence(), "the sequence should be exhausted")

	// The next call must block until the clock advances; drive it from another goroutine.
	done := make(chan ID, 1)
	go func() {
		id, err := g.Next()
		if err != nil {
			panic(err)
		}
		done <- id
	}()

	// Give the generator a moment to be genuinely waiting, then let time move.
	time.Sleep(20 * time.Millisecond)
	clock.advance(1)

	select {
	case id := <-done:
		assert.EqualValues(t, 7, id.Node(), "the sequence must never bleed into the node field")
		assert.EqualValues(t, 0, id.Sequence(), "a new millisecond restarts the sequence")
		assert.Greater(t, id, last)
	case <-time.After(5 * time.Second):
		t.Fatal("Next did not return after the clock advanced past the exhausted millisecond")
	}
}

func TestConcurrentGenerationProducesNoDuplicates(t *testing.T) {
	g, err := NewGenerator(3)
	require.NoError(t, err)

	const (
		goroutines = 16
		perG       = 2000
	)

	var (
		mu   sync.Mutex
		seen = make(map[ID]struct{}, goroutines*perG)
		wg   sync.WaitGroup
		errs atomic.Int64
	)

	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := make([]ID, 0, perG)
			for range perG {
				id, err := g.Next()
				if err != nil {
					errs.Add(1)
					return
				}
				local = append(local, id)
			}
			mu.Lock()
			defer mu.Unlock()
			for _, id := range local {
				seen[id] = struct{}{}
			}
		}()
	}
	wg.Wait()

	require.Zero(t, errs.Load(), "no generation should have failed")
	assert.Len(t, seen, goroutines*perG, "every concurrently-generated ID must be distinct")
}

func TestLayoutRoundTrips(t *testing.T) {
	g, _ := generatorAt(t, 512, 123456)

	id, err := g.Next()
	require.NoError(t, err)

	assert.EqualValues(t, 512, id.Node())
	assert.EqualValues(t, 0, id.Sequence())
	assert.Equal(t, time.UnixMilli(Epoch+123456).UTC(), id.Time())
	assert.Positive(t, int64(id), "the sign bit must stay clear so IDs fit Postgres bigint")
}

// A snowflake exceeds 2^53, so a JavaScript client parsing it as a JSON number silently loses precision and
// starts confusing adjacent IDs. Every client in this project sees a string.
func TestJSONMarshalsAsAQuotedString(t *testing.T) {
	id := ID(7238829238972837423)

	encoded, err := json.Marshal(id)
	require.NoError(t, err)
	assert.Equal(t, `"7238829238972837423"`, string(encoded))

	// Round-trip through a struct, which is how it actually travels.
	type payload struct {
		UserID ID `json:"user_id"`
	}
	out, err := json.Marshal(payload{UserID: id})
	require.NoError(t, err)
	assert.JSONEq(t, `{"user_id":"7238829238972837423"}`, string(out))

	var back payload
	require.NoError(t, json.Unmarshal(out, &back))
	assert.Equal(t, id, back.UserID, "the value must survive a JSON round trip exactly")
}

func TestJSONUnmarshalAcceptsABareNumber(t *testing.T) {
	// Leniency on input only: a hand-written curl request should not fail on something this cosmetic.
	var id ID
	require.NoError(t, json.Unmarshal([]byte(`7238829238972837423`), &id))
	assert.Equal(t, ID(7238829238972837423), id)
}

func TestJSONUnmarshalRejectsNonsense(t *testing.T) {
	for _, input := range []string{`"abc"`, `"12x"`, `{}`, `"  "`} {
		var id ID
		assert.Error(t, json.Unmarshal([]byte(input), &id), "input %s must be rejected", input)
	}
}

func TestParse(t *testing.T) {
	id, err := Parse("7238829238972837423")
	require.NoError(t, err)
	assert.Equal(t, ID(7238829238972837423), id)

	for _, bad := range []string{"", "abc", "-1", "9223372036854775808"} {
		_, err := Parse(bad)
		assert.Error(t, err, "input %q must be rejected", bad)
	}
}

func TestStringMatchesTheDecimalForm(t *testing.T) {
	assert.Equal(t, "7238829238972837423", ID(7238829238972837423).String())
	assert.Equal(t, "0", ID(0).String())
}

func BenchmarkNext(b *testing.B) {
	g, err := NewGenerator(1)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := g.Next(); err != nil {
			b.Fatal(err)
		}
	}
}
