// Tests of the ULID mint. Why: the daemon's ids are its idempotency keys, so they must be valid,
// unique and sortable by time.

package ulid

import (
	"sort"
	"testing"
	"time"
)

// A journal id is the idempotency key and must sort in arrival order, even for a burst inside
// one millisecond.
func TestBurstInOneMillisecondStaysOrdered(t *testing.T) {
	now := time.Now()
	var ids []string
	for i := 0; i < 1000; i++ {
		ids = append(ids, At(now))
	}
	if !sort.StringsAreSorted(ids) {
		t.Fatal("ids not increasing")
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if !Valid(id) || seen[id] {
			t.Fatalf("bad or duplicate id %q", id)
		}
		seen[id] = true
	}
}

func TestLaterTimeSortsLater(t *testing.T) {
	a := At(time.Now())
	b := At(time.Now().Add(time.Second))
	if !(a < b) {
		t.Fatalf("%s !< %s", a, b)
	}
}
