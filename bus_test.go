package main

import (
	"sync"
	"testing"

	"github.com/tachyne/tachyne-map/render"
	"github.com/tachyne/tachyne-world/worldread"
)

// testServer builds the minimum server applyBlockChange touches: a terrain-only
// world it can mutate in memory, plus a live hub to mark tiles dirty on.
func testServer(t *testing.T) *server {
	t.Helper()
	reader, err := worldread.Open(worldread.Overworld, 1, nil)
	if err != nil {
		t.Fatalf("open world: %v", err)
	}
	return &server{reader: reader, live: newLiveHub(), cache: map[[2]int]*render.Tile{}}
}

// stateAt reads one block through the chunk view, the same way the mesher
// does — worldread has no single-block accessor.
func stateAt(t *testing.T, s *server, x, y, z int) uint32 {
	t.Helper()
	c := s.reader.Chunk(floorDiv(x, 16), floorDiv(z, 16))
	if c == nil {
		t.Fatalf("no chunk for %d,%d", x, z)
	}
	lx, lz := x-floorDiv(x, 16)*16, z-floorDiv(z, 16)*16
	return c.State(lx, y-worldread.MinY, lz)
}

// Edits that arrive before the snapshot has been read must be held, not
// dropped and not applied to a world that doesn't exist yet. This is the whole
// point of the type: it closes the window in which a player's build would land
// in neither the world file nor the event stream.
func TestBootFollowerBuffersUntilAttach(t *testing.T) {
	srv := testServer(t)
	f := &bootFollower{}

	const (
		x, y, z = 40, 70, -20
		state   = uint32(9)
	)
	before := stateAt(t, srv, x, y, z)
	if before == state {
		t.Fatalf("test picked a state the world already has (%d) — choose another", state)
	}

	f.handle(blockChange{X: x, Y: y, Z: z, State: state})

	// Nothing may have been applied yet: there was no server to apply it to.
	if got := stateAt(t, srv, x, y, z); got != before {
		t.Fatalf("edit applied before attach: got %d, want %d", got, before)
	}

	if n := f.attach(srv); n != 1 {
		t.Fatalf("attach drained %d edits, want 1", n)
	}
	if got := stateAt(t, srv, x, y, z); got != state {
		t.Fatalf("buffered edit not applied on attach: got %d, want %d", got, state)
	}
	if len(f.buf) != 0 {
		t.Errorf("buffer not released after attach: %d entries", len(f.buf))
	}
}

// After attach the follower applies straight through rather than growing the
// buffer forever.
func TestBootFollowerAppliesDirectlyAfterAttach(t *testing.T) {
	srv := testServer(t)
	f := &bootFollower{}
	f.attach(srv)

	const x, y, z = 12, 72, 34
	f.handle(blockChange{X: x, Y: y, Z: z, State: 5})

	if got := stateAt(t, srv, x, y, z); got != 5 {
		t.Fatalf("post-attach edit not applied: got %d, want 5", got)
	}
	if len(f.buf) != 0 {
		t.Errorf("post-attach edit was buffered: %d entries", len(f.buf))
	}
}

// Buffered edits must be replayed in arrival order: two writes to the same
// block must leave the LAST one standing, not whichever the map happened to
// apply second.
func TestBootFollowerPreservesOrder(t *testing.T) {
	srv := testServer(t)
	f := &bootFollower{}

	const x, y, z = -8, 65, 3
	f.handle(blockChange{X: x, Y: y, Z: z, State: 1})
	f.handle(blockChange{X: x, Y: y, Z: z, State: 2})
	f.handle(blockChange{X: x, Y: y, Z: z, State: 3})
	f.attach(srv)

	if got := stateAt(t, srv, x, y, z); got != 3 {
		t.Fatalf("replay lost ordering: got %d, want 3 (the last edit)", got)
	}
}

// Events keep arriving while attach is draining. Every edit must survive, and
// the two paths must not deadlock against each other.
func TestBootFollowerAttachRacesWithIncomingEdits(t *testing.T) {
	srv := testServer(t)
	f := &bootFollower{}

	const n = 200
	for i := 0; i < n; i++ { // pre-attach backlog
		f.handle(blockChange{X: i, Y: 70, Z: 0, State: uint32(i + 1)})
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ { // concurrent arrivals
			f.handle(blockChange{X: i, Y: 71, Z: 0, State: uint32(i + 1)})
		}
	}()
	f.attach(srv)
	wg.Wait()

	// Anything still buffered was queued behind attach; drain it the way a
	// second attach would never need to.
	f.mu.Lock()
	leftover := len(f.buf)
	f.mu.Unlock()
	if leftover != 0 {
		t.Errorf("edits stranded in the buffer after attach: %d", leftover)
	}

	for i := 0; i < n; i++ {
		if got := stateAt(t, srv, i, 70, 0); got != uint32(i+1) {
			t.Fatalf("backlog edit %d lost: got %d, want %d", i, got, i+1)
		}
		if got := stateAt(t, srv, i, 71, 0); got != uint32(i+1) {
			t.Fatalf("concurrent edit %d lost: got %d, want %d", i, got, i+1)
		}
	}
}
