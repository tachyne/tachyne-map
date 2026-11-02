package main

import "testing"

// The cache keeps tiles by bytes, least recently used out first: a tile
// that is read stays while older ones go.
func TestTileCacheEvictsLeastRecentlyUsed(t *testing.T) {
	c := newTileCache(30)
	a, b, d := [2]int{0, 0}, [2]int{1, 0}, [2]int{2, 0}
	c.put(a, 0, make([]byte, 10))
	c.put(b, 0, make([]byte, 10))
	c.get(a) // a is now the most recent
	c.put(d, 0, make([]byte, 15))
	if _, ok := c.get(b); ok {
		t.Error("b, the least recently used, should have gone")
	}
	if _, ok := c.get(a); !ok {
		t.Error("a was read since b, and should have stayed")
	}
	if _, ok := c.get(d); !ok {
		t.Error("d, just put, is missing")
	}
}

// An invalidated tile leaves the cache and its generation moves on, so its
// ETag changes; a mesh started before the change is not cached after it.
func TestTileCacheInvalidation(t *testing.T) {
	c := newTileCache(1 << 20)
	k := [2]int{3, 4}
	g := c.generation(k)
	c.put(k, g, []byte("old"))
	c.invalidate([]tileKey{tileKey(k)})
	if _, ok := c.get(k); ok {
		t.Error("an invalidated tile is still cached")
	}
	if c.generation(k) == g {
		t.Error("the generation did not move on")
	}
	c.put(k, g, []byte("meshed before the change"))
	if _, ok := c.get(k); ok {
		t.Error("a tile meshed before the change was cached after it")
	}
	c.put(k, c.generation(k), []byte("new"))
	if gz, ok := c.get(k); !ok || string(gz) != "new" {
		t.Errorf("the fresh tile: %q, %v", gz, ok)
	}
}
