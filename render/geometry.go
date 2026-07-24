package render

import (
	"image/color"
	"math"
	"strings"
)

// dir is a face direction with its unit normal.
type dir struct {
	nx, ny, nz int
}

// faceDirs maps Minecraft face/cullface names to unit normals.
var faceDirs = map[string]dir{
	"down":  {0, -1, 0},
	"up":    {0, 1, 0},
	"north": {0, 0, -1},
	"south": {0, 0, 1},
	"west":  {-1, 0, 0},
	"east":  {1, 0, 0},
}

// boxFace returns the four corners (Minecraft model space, 0..16) and their
// texture UVs (0..16, V measured from the top) for one face of an axis-aligned
// box. Corners are wound counter-clockwise viewed from OUTSIDE, so after the
// (rotation-only) transforms the quad renders as a front face. UVs are the
// vanilla auto-UV derived from the element bounds; explicit per-face UV is a
// TODO (rare on terrain, where auto-UV is exact).
func boxFace(from, to [3]float64, face string, _ *[4]float64) ([4][3]float64, [4][2]float64) {
	x0, y0, z0 := from[0], from[1], from[2]
	x1, y1, z1 := to[0], to[1], to[2]
	switch face {
	case "up":
		return [4][3]float64{{x0, y1, z0}, {x0, y1, z1}, {x1, y1, z1}, {x1, y1, z0}},
			[4][2]float64{{x0, z0}, {x0, z1}, {x1, z1}, {x1, z0}}
	case "down":
		return [4][3]float64{{x0, y0, z0}, {x1, y0, z0}, {x1, y0, z1}, {x0, y0, z1}},
			[4][2]float64{{x0, z0}, {x1, z0}, {x1, z1}, {x0, z1}}
	case "north":
		return [4][3]float64{{x1, y0, z0}, {x0, y0, z0}, {x0, y1, z0}, {x1, y1, z0}},
			[4][2]float64{{x1, 16 - y0}, {x0, 16 - y0}, {x0, 16 - y1}, {x1, 16 - y1}}
	case "south":
		return [4][3]float64{{x0, y0, z1}, {x1, y0, z1}, {x1, y1, z1}, {x0, y1, z1}},
			[4][2]float64{{x0, 16 - y0}, {x1, 16 - y0}, {x1, 16 - y1}, {x0, 16 - y1}}
	case "west":
		return [4][3]float64{{x0, y0, z0}, {x0, y0, z1}, {x0, y1, z1}, {x0, y1, z0}},
			[4][2]float64{{z0, 16 - y0}, {z1, 16 - y0}, {z1, 16 - y1}, {z0, 16 - y1}}
	case "east":
		return [4][3]float64{{x1, y0, z1}, {x1, y0, z0}, {x1, y1, z0}, {x1, y1, z1}},
			[4][2]float64{{z1, 16 - y0}, {z0, 16 - y0}, {z0, 16 - y1}, {z1, 16 - y1}}
	}
	return [4][3]float64{}, [4][2]float64{}
}

// applyTransforms applies an element's own rotation (about its origin) then the
// blockstate model rotation (ref.X/ref.Y about the block centre) to a point.
// Model rotations use negative angles to match vanilla's BlockModelRotation.
func applyTransforms(p [3]float64, elRot *ElementRotation, ref ModelRef) [3]float64 {
	if elRot != nil {
		p = rotateAround(p, elRot.Axis, elRot.Angle, elRot.Origin)
	}
	center := [3]float64{8, 8, 8}
	if ref.X != 0 {
		p = rotateAround(p, "x", -float64(ref.X), center)
	}
	if ref.Y != 0 {
		p = rotateAround(p, "y", -float64(ref.Y), center)
	}
	return p
}

// transformNormal applies the same rotations as applyTransforms about the
// origin (direction only, no translation).
func transformNormal(n [3]float64, elRot *ElementRotation, ref ModelRef) [3]float64 {
	zero := [3]float64{}
	if elRot != nil {
		n = rotateAround(n, elRot.Axis, elRot.Angle, zero)
	}
	if ref.X != 0 {
		n = rotateAround(n, "x", -float64(ref.X), zero)
	}
	if ref.Y != 0 {
		n = rotateAround(n, "y", -float64(ref.Y), zero)
	}
	return n
}

// rotateAround rotates p by deg degrees about the axis ("x"/"y"/"z") through
// origin (right-handed).
func rotateAround(p [3]float64, axis string, deg float64, origin [3]float64) [3]float64 {
	rad := deg * math.Pi / 180
	s, c := math.Sin(rad), math.Cos(rad)
	x, y, z := p[0]-origin[0], p[1]-origin[1], p[2]-origin[2]
	switch axis {
	case "x":
		y, z = y*c-z*s, y*s+z*c
	case "y":
		x, z = x*c+z*s, -x*s+z*c
	case "z":
		x, y = x*c-y*s, x*s+y*c
	}
	return [3]float64{x + origin[0], y + origin[1], z + origin[2]}
}

// dominantAxis snaps a (possibly rotated) normal to the nearest unit axis.
func dominantAxis(n [3]float64) [3]int {
	ax, ay, az := math.Abs(n[0]), math.Abs(n[1]), math.Abs(n[2])
	switch {
	case ax >= ay && ax >= az && ax > 1e-6:
		return [3]int{sign(n[0]), 0, 0}
	case ay >= az && ay > 1e-6:
		return [3]int{0, sign(n[1]), 0}
	case az > 1e-6:
		return [3]int{0, 0, sign(n[2])}
	}
	return [3]int{}
}

func sign(v float64) int {
	if v > 0 {
		return 1
	}
	if v < 0 {
		return -1
	}
	return 0
}

// faceShade is vanilla's fixed directional face shading (top brightest, bottom
// darkest), applied by world-space normal so it survives rotation.
func faceShade(normal [3]int) float32 {
	switch {
	case normal[1] > 0:
		return 1.0
	case normal[1] < 0:
		return 0.5
	case normal[0] != 0:
		return 0.6
	case normal[2] != 0:
		return 0.8
	}
	return 0.8
}

// isFullCube reports whether a model has a full 0..16 element (used for
// occlusion / face culling).
func isFullCube(model *Model) bool {
	for _, el := range model.Elements {
		if el.From == ([3]float64{0, 0, 0}) && el.To == ([3]float64{16, 16, 16}) {
			return true
		}
	}
	return false
}

func isFluid(name string) bool {
	return name == "minecraft:water" || name == "minecraft:lava"
}

// opaqueName reports whether a full-cube block also blocks sight (so it may cull
// the neighbour's facing quad). Translucent full cubes (glass, ice, …) do not.
func opaqueName(name string) bool {
	switch {
	case strings.Contains(name, "glass"):
		return false
	case strings.HasSuffix(name, "ice"): // ice, packed_ice, blue_ice, frosted_ice
		return false
	case name == "minecraft:slime_block", name == "minecraft:honey_block",
		name == "minecraft:barrier", name == "minecraft:structure_void",
		name == "minecraft:light":
		return false
	}
	return true
}

// tintKind classifies a tint-carrying face by its block name.
func tintKind(name string, tintIndex *int) int {
	if tintIndex == nil {
		return tintNone
	}
	switch {
	case name == "minecraft:water":
		return tintWater
	case strings.Contains(name, "leaves"), strings.Contains(name, "vine"):
		return tintFoliage
	case strings.Contains(name, "grass"), strings.Contains(name, "fern"),
		strings.Contains(name, "sugar_cane"):
		return tintGrass
	}
	return tintGrass // most other tintindex blocks read as grass-like
}

// biomeClimate returns a biome's (temperature, downfall) for colormap sampling.
// A modest table for common biomes; unknowns default to plains.
func biomeClimate(biome string) (float64, float64) {
	n := strings.TrimPrefix(biome, "minecraft:")
	if t, ok := biomeClimates[n]; ok {
		return t[0], t[1]
	}
	switch {
	case strings.Contains(n, "snowy"), strings.Contains(n, "frozen"), strings.Contains(n, "ice"):
		return 0.0, 0.5
	case strings.Contains(n, "desert"), strings.Contains(n, "badlands"), strings.Contains(n, "savanna"):
		return 2.0, 0.0
	case strings.Contains(n, "jungle"):
		return 0.95, 0.9
	case strings.Contains(n, "swamp"):
		return 0.8, 0.9
	case strings.Contains(n, "taiga"):
		return 0.25, 0.8
	}
	return 0.8, 0.4 // plains
}

var biomeClimates = map[string][2]float64{
	"plains":           {0.8, 0.4},
	"sunflower_plains": {0.8, 0.4},
	"forest":           {0.7, 0.8},
	"flower_forest":    {0.7, 0.8},
	"birch_forest":     {0.6, 0.6},
	"dark_forest":      {0.7, 0.8},
	"taiga":            {0.25, 0.8},
	"snowy_taiga":      {-0.5, 0.4},
	"desert":           {2.0, 0.0},
	"savanna":          {2.0, 0.0},
	"badlands":         {2.0, 0.0},
	"jungle":           {0.95, 0.9},
	"swamp":            {0.8, 0.9},
	"mangrove_swamp":   {0.8, 0.9},
	"snowy_plains":     {0.0, 0.5},
	"ocean":            {0.5, 0.5},
	"deep_ocean":       {0.5, 0.5},
	"cold_ocean":       {0.5, 0.5},
	"warm_ocean":       {0.5, 0.5},
	"beach":            {0.8, 0.4},
	"cherry_grove":     {0.5, 0.8},
	"mushroom_fields":  {0.9, 1.0},
	"windswept_hills":  {0.2, 0.3},
	"meadow":           {0.5, 0.8},
}

// waterColor returns the water tint (0..1 RGB) for a biome (a few overrides;
// default overworld blue #3F76E4).
func waterColor(biome string) [3]float32 {
	n := strings.TrimPrefix(biome, "minecraft:")
	switch {
	case strings.Contains(n, "swamp"):
		return [3]float32{0.38, 0.48, 0.39} // #617B64
	case strings.Contains(n, "warm_ocean"), strings.Contains(n, "lukewarm"):
		return [3]float32{0.27, 0.60, 0.83}
	case strings.Contains(n, "cold_ocean"), strings.Contains(n, "frozen"):
		return [3]float32{0.24, 0.34, 0.74}
	}
	return [3]float32{0.247, 0.463, 0.894} // #3F76E4
}

func rgb01(c color.RGBA) [3]float32 {
	return [3]float32{float32(c.R) / 255, float32(c.G) / 255, float32(c.B) / 255}
}

// lightLevel maps a 0..15 light level to a brightness multiplier, with a floor
// so deep-dark blocks stay faintly visible on the map.
func lightLevel(l uint8) float32 {
	return 0.1 + 0.9*(float32(l)/15)
}

func maxu8(a, b uint8) uint8 {
	if a > b {
		return a
	}
	return b
}
