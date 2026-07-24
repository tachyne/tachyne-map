package render

import (
	"github.com/tachyne/tachyne-world/worldread"
)

// Mesher turns worldread chunks into Tiles. It resolves each block-state id to
// its vanilla model(s), bakes per-face geometry once per distinct state (the
// expensive part — rotation, UV mapping, atlas lookup), then instances those
// quads cheaply across the chunk with per-block face culling and baked
// light/tint/shade.
type Mesher struct {
	assets *Assets
	atlas  *Atlas
	cm     *Colormaps

	bsCache    map[string]*RawBlockState // block name -> blockstate (nil = none)
	modelCache map[string]*Model         // model loc -> resolved model
	quadCache  map[uint32][]quadTemplate // state id -> baked quads
	occCache   map[uint32]bool           // state id -> occludes a neighbour face
	nameCache  map[uint32]string         // state id -> block name
}

// NewMesher builds a mesher over the given assets, atlas, and colormaps.
func NewMesher(assets *Assets, atlas *Atlas, cm *Colormaps) *Mesher {
	return &Mesher{
		assets:     assets,
		atlas:      atlas,
		cm:         cm,
		bsCache:    map[string]*RawBlockState{},
		modelCache: map[string]*Model{},
		quadCache:  map[uint32][]quadTemplate{},
		occCache:   map[uint32]bool{},
		nameCache:  map[uint32]string{},
	}
}

// FluidTextures are the extra textures the atlas must contain for fluid
// rendering (fluids have no vanilla block model, so they are synthesised).
var FluidTextures = []string{"minecraft:block/water_still", "minecraft:block/lava_still"}

// tint kinds for a face carrying a tintindex.
const (
	tintNone = iota
	tintGrass
	tintFoliage
	tintWater
)

// quadTemplate is one face of a block in block-local space (0..1), with atlas
// UVs and the metadata needed to instance it: the world-space geometric normal
// (for light + shade), the cull direction (zero = never cull), the tint kind,
// and the precomputed directional shade.
type quadTemplate struct {
	pos    [4][3]float32
	uv     [4][2]float32
	normal [3]int
	cull   [3]int
	tint   int
	shade  float32
}

// MeshChunk renders one chunk into a Tile, culling faces against neighbouring
// blocks (fetched read-through from r) and baking light/tint/shade per face.
func (m *Mesher) MeshChunk(r *worldread.Reader, cx, cz int) *Tile {
	view := newChunkView(r, cx, cz)
	center := view.chunk(cx, cz)
	tile := NewTile(r.Dim().String(), cx, cz)
	height := center.Height()

	for lx := 0; lx < 16; lx++ {
		for lz := 0; lz < 16; lz++ {
			wx, wz := cx*16+lx, cz*16+lz
			for ly := 0; ly < height; ly++ {
				state := center.State(lx, ly, lz)
				if state == 0 {
					continue
				}
				quads := m.stateQuads(state)
				if len(quads) == 0 {
					continue
				}
				wy := worldread.MinY + ly
				biome := center.Biome(ly)
				for _, q := range quads {
					if q.cull != [3]int{} {
						nb := view.state(wx+q.cull[0], wy+q.cull[1], wz+q.cull[2])
						if m.occludes(nb) {
							continue
						}
					}
					// Sample light on the exposed (air) side of the face.
					sx, sy, sz := wx+q.normal[0], wy+q.normal[1], wz+q.normal[2]
					if q.normal == [3]int{} {
						sx, sy, sz = wx, wy, wz
					}
					sky := view.skyLight(sx, sy, sz)
					blk := view.blockLight(sx, sy, sz)
					lum := lightLevel(maxu8(sky, blk))
					base := q.shade * lum

					col := [3]float32{base, base, base}
					if q.tint != tintNone {
						t := m.tintColor(q.tint, biome)
						col[0] *= t[0]
						col[1] *= t[1]
						col[2] *= t[2]
					}

					var p [4][3]float32
					for i := 0; i < 4; i++ {
						p[i] = [3]float32{
							float32(wx) + q.pos[i][0],
							float32(wy) + q.pos[i][1],
							float32(wz) + q.pos[i][2],
						}
					}
					tile.AddQuad(p, q.uv, col)
				}
			}
		}
	}
	return tile
}

// stateQuads returns (and caches) the baked face quads for a block state.
func (m *Mesher) stateQuads(state uint32) []quadTemplate {
	if q, ok := m.quadCache[state]; ok {
		return q
	}
	q := m.buildStateQuads(state)
	m.quadCache[state] = q
	return q
}

func (m *Mesher) buildStateQuads(state uint32) []quadTemplate {
	name, props := worldread.Decode(state)
	m.nameCache[state] = name
	if name == "minecraft:air" {
		return nil
	}
	if isFluid(name) {
		return m.fluidQuads(name)
	}
	bs := m.blockState(name)
	if bs == nil {
		return nil
	}
	var out []quadTemplate
	for _, ref := range bs.Chosen(props) {
		model := m.model(ref.Model)
		if model == nil {
			continue
		}
		for i := range model.Elements {
			out = append(out, m.elementQuads(name, model, &model.Elements[i], ref)...)
		}
	}
	return out
}

// elementQuads bakes the visible faces of one element under a blockstate model
// rotation (ref.X/ref.Y) into quad templates.
func (m *Mesher) elementQuads(name string, model *Model, el *Element, ref ModelRef) []quadTemplate {
	var out []quadTemplate
	for faceName, face := range el.Faces {
		d, ok := faceDirs[faceName]
		if !ok {
			continue
		}
		loc := model.ResolveTexture(face.Texture)
		spr := m.atlas.Lookup(loc)

		corners, uvs := boxFace(el.From, el.To, faceName, face.UV)

		// Transform: element rotation (about its origin), then blockstate model
		// rotation (about the block centre 8,8,8), applied to corners + normal.
		var wp [4][3]float32
		for i, c := range corners {
			c = applyTransforms(c, el.Rotation, ref)
			wp[i] = [3]float32{float32(c[0] / 16), float32(c[1] / 16), float32(c[2] / 16)}
		}
		nrm := transformNormal([3]float64{float64(d.nx), float64(d.ny), float64(d.nz)}, el.Rotation, ref)
		normal := dominantAxis(nrm)

		cull := [3]int{}
		if face.CullFace != "" {
			if cd, ok := faceDirs[face.CullFace]; ok {
				cn := transformNormal([3]float64{float64(cd.nx), float64(cd.ny), float64(cd.nz)}, el.Rotation, ref)
				cull = dominantAxis(cn)
			}
		}

		var uv [4][2]float32
		for i := range uvs {
			uv[i] = [2]float32{
				float32(spr.U0 + (uvs[i][0]/16)*(spr.U1-spr.U0)),
				float32(spr.V0 + (uvs[i][1]/16)*(spr.V1-spr.V0)),
			}
		}

		out = append(out, quadTemplate{
			pos:    wp,
			uv:     uv,
			normal: normal,
			cull:   cull,
			tint:   tintKind(name, face.TintIndex),
			shade:  faceShade(normal),
		})
	}
	return out
}

// fluidQuads synthesises a full-cube render for water/lava (they have no vanilla
// block model). Water carries a tint; lava does not.
func (m *Mesher) fluidQuads(name string) []quadTemplate {
	loc := "minecraft:block/water_still"
	tint := tintWater
	if name == "minecraft:lava" {
		loc, tint = "minecraft:block/lava_still", tintNone
	}
	spr := m.atlas.Lookup(loc)
	from, to := [3]float64{0, 0, 0}, [3]float64{16, 16, 16}
	var out []quadTemplate
	for faceName, d := range faceDirs {
		corners, uvs := boxFace(from, to, faceName, nil)
		var wp [4][3]float32
		for i, c := range corners {
			wp[i] = [3]float32{float32(c[0] / 16), float32(c[1] / 16), float32(c[2] / 16)}
		}
		normal := [3]int{d.nx, d.ny, d.nz}
		var uv [4][2]float32
		for i := range uvs {
			uv[i] = [2]float32{
				float32(spr.U0 + (uvs[i][0]/16)*(spr.U1-spr.U0)),
				float32(spr.V0 + (uvs[i][1]/16)*(spr.V1-spr.V0)),
			}
		}
		out = append(out, quadTemplate{
			pos: wp, uv: uv, normal: normal, cull: normal, tint: tint, shade: faceShade(normal),
		})
	}
	return out
}

// occludes reports whether a block fully hides an adjacent face: a full opaque
// cube, or a fluid (so fluid interiors don't over-render).
func (m *Mesher) occludes(state uint32) bool {
	if state == 0 {
		return false
	}
	if v, ok := m.occCache[state]; ok {
		return v
	}
	name, props := worldread.Decode(state)
	v := false
	if isFluid(name) {
		v = true
	} else if opaqueName(name) {
		if bs := m.blockState(name); bs != nil {
			for _, ref := range bs.Chosen(props) {
				if model := m.model(ref.Model); model != nil && isFullCube(model) {
					v = true
					break
				}
			}
		}
	}
	m.occCache[state] = v
	return v
}

// blockState fetches and caches a block's blockstate JSON (nil if none).
func (m *Mesher) blockState(name string) *RawBlockState {
	if bs, ok := m.bsCache[name]; ok {
		return bs
	}
	bs, err := m.assets.BlockState(name)
	if err != nil {
		bs = nil
	}
	m.bsCache[name] = bs
	return bs
}

// model resolves and caches a model by location (nil on failure).
func (m *Mesher) model(loc string) *Model {
	if mo, ok := m.modelCache[loc]; ok {
		return mo
	}
	mo, err := Resolve(m.assets, loc)
	if err != nil {
		mo = nil
	}
	m.modelCache[loc] = mo
	return mo
}

// tintColor returns the RGB multiplier (0..1) for a tint kind in a biome.
func (m *Mesher) tintColor(kind int, biome string) [3]float32 {
	switch kind {
	case tintGrass:
		t, d := biomeClimate(biome)
		return rgb01(m.cm.Grass(t, d))
	case tintFoliage:
		t, d := biomeClimate(biome)
		return rgb01(m.cm.Foliage(t, d))
	case tintWater:
		return waterColor(biome)
	}
	return [3]float32{1, 1, 1}
}
