package render

import (
	"compress/gzip"
	"encoding/json"
	"io"
)

// Tile is one chunk's baked geometry, ready for the WebGL viewer.
//
// Design choice: lighting and biome tint are pre-multiplied into Colors, so the
// viewer needs only the atlas texture and per-vertex colors — no runtime
// lighting, no normals. Positions are absolute world coordinates; UVs index the
// shared atlas (0..1, V from the top, matching Minecraft model UVs). This is a
// deliberately simple, documented contract (contrast BlueMap's opaque .prbm):
// the viewer builds one BufferGeometry per tile with
// MeshBasicMaterial{map: atlas, vertexColors: true}.
type Tile struct {
	Dim       string    `json:"dim"`
	CX        int       `json:"cx"`
	CZ        int       `json:"cz"`
	Positions []float32 `json:"positions"` // x,y,z triples (world coords)
	UVs       []float32 `json:"uvs"`       // u,v pairs into the atlas
	Colors    []float32 `json:"colors"`    // r,g,b triples, 0..1 (light×tint baked)
	Indices   []uint32  `json:"indices"`   // triangle indices (3 per triangle)
}

// NewTile returns an empty tile for the given chunk.
func NewTile(dim string, cx, cz int) *Tile {
	return &Tile{Dim: dim, CX: cx, CZ: cz}
}

// Empty reports whether the tile has no geometry.
func (t *Tile) Empty() bool { return len(t.Indices) == 0 }

// VertexCount is the number of vertices currently in the tile.
func (t *Tile) VertexCount() int { return len(t.Positions) / 3 }

// AddQuad appends one textured, colored quad as two triangles. Vertices must be
// wound counter-clockwise when viewed from the visible (front) side. p are the
// four corner world positions, uv the matching atlas coordinates, and col the
// per-quad baked color (light×tint) applied to all four vertices.
func (t *Tile) AddQuad(p [4][3]float32, uv [4][2]float32, col [3]float32) {
	base := uint32(t.VertexCount())
	for i := 0; i < 4; i++ {
		t.Positions = append(t.Positions, p[i][0], p[i][1], p[i][2])
		t.UVs = append(t.UVs, uv[i][0], uv[i][1])
		t.Colors = append(t.Colors, col[0], col[1], col[2])
	}
	// 0-1-2, 0-2-3
	t.Indices = append(t.Indices,
		base+0, base+1, base+2,
		base+0, base+2, base+3)
}

// EncodeJSON writes the tile as compact JSON.
func (t *Tile) EncodeJSON(w io.Writer) error {
	return json.NewEncoder(w).Encode(t)
}

// EncodeGZ writes the tile as gzipped JSON (what the pod serves; the viewer
// sends Accept-Encoding: gzip and the browser inflates transparently).
func (t *Tile) EncodeGZ(w io.Writer) error {
	gz := gzip.NewWriter(w)
	if err := json.NewEncoder(gz).Encode(t); err != nil {
		gz.Close()
		return err
	}
	return gz.Close()
}
