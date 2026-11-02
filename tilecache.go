package main

import (
	"bytes"
	"container/list"
	"sync"
)

// tileCacheBudget bounds the meshed-tile cache by the bytes it holds. Tiles
// are kept as the gzipped JSON the pod serves (~40 KB each, against ~0.5 MB
// of geometry as a render.Tile), so the cache covers the viewer's whole
// streamed ring and the places it returns to — several thousand tiles —
// instead of the 256 that held geometry and was evicted as fast as panning
// filled it, re-meshing tiles the viewer had only just dropped.
const tileCacheBudget = 192 << 20

// tileCache is a least-recently-used cache of encoded tiles, with a
// generation per tile that changes whenever the world changes under it: the
// tile's ETag, so a browser revisiting an unchanged tile is answered with a
// 304 and nothing is meshed or sent.
type tileCache struct {
	mu     sync.Mutex
	budget int
	bytes  int
	lru    *list.List // front = most recently used; values are [2]int keys
	items  map[[2]int]*cachedTile
	gen    map[[2]int]uint32
}

type cachedTile struct {
	gz   []byte
	elem *list.Element
}

func newTileCache(budget int) *tileCache {
	return &tileCache{budget: budget, lru: list.New(), items: map[[2]int]*cachedTile{}, gen: map[[2]int]uint32{}}
}

// get returns a tile's gzipped bytes, marking it recently used.
func (c *tileCache) get(k [2]int) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.items[k]
	if !ok {
		return nil, false
	}
	c.lru.MoveToFront(t.elem)
	return t.gz, true
}

// put stores a tile meshed at generation g; a tile invalidated while it was
// being meshed (its generation has moved on) is not kept.
func (c *tileCache) put(k [2]int, g uint32, gz []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gen[k] != g {
		return
	}
	if old, ok := c.items[k]; ok {
		c.bytes -= len(old.gz)
		c.lru.Remove(old.elem)
	}
	c.items[k] = &cachedTile{gz: gz, elem: c.lru.PushFront(k)}
	c.bytes += len(gz)
	for c.bytes > c.budget && c.lru.Len() > 1 {
		back := c.lru.Back()
		bk := back.Value.([2]int)
		c.bytes -= len(c.items[bk].gz)
		delete(c.items, bk)
		c.lru.Remove(back)
	}
}

// generation is a tile's current generation (its ETag's tail).
func (c *tileCache) generation(k [2]int) uint32 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gen[k]
}

// invalidate drops tiles the world has changed under and moves their
// generation on, so cached copies anywhere — here and in browsers — are stale.
func (c *tileCache) invalidate(keys []tileKey) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, k := range keys {
		c.gen[k]++
		if t, ok := c.items[k]; ok {
			c.bytes -= len(t.gz)
			c.lru.Remove(t.elem)
			delete(c.items, k)
		}
	}
}

// len and size report the cache's occupancy (tests, logs).
func (c *tileCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items)
}

func gzBytes(encode func(w *bytes.Buffer) error) ([]byte, error) {
	var b bytes.Buffer
	if err := encode(&b); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}
