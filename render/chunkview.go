package render

import "github.com/tachyne/tachyne-world/worldread"

// chunkView is a small read-through window over a chunk and its neighbours,
// caching generated chunks so face-culling and light sampling across chunk
// borders don't re-generate a neighbour per lookup. (The underlying world keeps
// its own LRU cache too, so cross-tile neighbour reuse is cheap.)
type chunkView struct {
	r     *worldread.Reader
	cache map[[2]int]*worldread.Chunk
}

func newChunkView(r *worldread.Reader, cx, cz int) *chunkView {
	v := &chunkView{r: r, cache: map[[2]int]*worldread.Chunk{}}
	v.chunk(cx, cz) // warm the centre
	return v
}

func (v *chunkView) chunk(cx, cz int) *worldread.Chunk {
	key := [2]int{cx, cz}
	if c, ok := v.cache[key]; ok {
		return c
	}
	c := v.r.Chunk(cx, cz)
	v.cache[key] = c
	return c
}

// at resolves world coordinates to the owning chunk and local block indices
// (ly is 0-based from the world floor).
func (v *chunkView) at(wx, wy, wz int) (*worldread.Chunk, int, int, int) {
	cx, cz := floorDiv(wx, 16), floorDiv(wz, 16)
	c := v.chunk(cx, cz)
	return c, wx - cx*16, wy - worldread.MinY, wz - cz*16
}

func (v *chunkView) state(wx, wy, wz int) uint32 {
	c, lx, ly, lz := v.at(wx, wy, wz)
	return c.State(lx, ly, lz)
}

func (v *chunkView) skyLight(wx, wy, wz int) uint8 {
	c, lx, ly, lz := v.at(wx, wy, wz)
	return c.SkyLightAt(lx, ly, lz)
}

func (v *chunkView) blockLight(wx, wy, wz int) uint8 {
	c, lx, ly, lz := v.at(wx, wy, wz)
	return c.BlockLightAt(lx, ly, lz)
}

// floorDiv is integer division rounding toward negative infinity (correct chunk
// index for negative world coordinates).
func floorDiv(a, b int) int {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}
