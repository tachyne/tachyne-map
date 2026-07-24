// Package render is tachyne-map's rendering library: it provisions the vanilla
// client assets, parses Mojang's blockstate/block-model JSON, and (later
// milestones) stitches a texture atlas and meshes the engine's chunks into the
// tiles served by the map pod.
//
// This file implements the block-model layer: the on-disk JSON shapes
// (models/block/*.json) and their resolution — Mojang models form an
// inheritance chain via "parent", and reference textures indirectly through
// "#name" variables that must be chased to a concrete texture location.
package render

import "strings"

// RawModel is one block-model JSON file exactly as authored, before parent
// flattening. Optional scalars are pointers so "absent" is distinguishable
// from "zero" (e.g. ambientocclusion defaults to true, not false).
type RawModel struct {
	Parent           string            `json:"parent"`
	AmbientOcclusion *bool             `json:"ambientocclusion"`
	Textures         map[string]string `json:"textures"`
	Elements         []Element         `json:"elements"`
}

// Element is one box of a model, in Mojang's 0..16 model space.
type Element struct {
	From     [3]float64       `json:"from"`
	To       [3]float64       `json:"to"`
	Rotation *ElementRotation `json:"rotation"`
	Shade    *bool            `json:"shade"`
	Faces    map[string]Face  `json:"faces"` // keyed by direction: down/up/north/south/west/east
}

// ElementRotation rotates a single element about one axis.
type ElementRotation struct {
	Origin  [3]float64 `json:"origin"`
	Axis    string     `json:"axis"` // x/y/z
	Angle   float64    `json:"angle"`
	Rescale bool       `json:"rescale"`
}

// Face is one textured face of an element. Texture is a "#name" variable or a
// concrete location; resolve it with Model.ResolveTexture.
type Face struct {
	UV        *[4]float64 `json:"uv"` // [x1,y1,x2,y2] in 0..16; absent = derive from element bounds
	Texture   string      `json:"texture"`
	CullFace  string      `json:"cullface"` // face culled when a full block abuts this side
	Rotation  int         `json:"rotation"` // 0/90/180/270
	TintIndex *int        `json:"tintindex"`
}

// Model is a fully resolved block model: parent chain flattened, textures
// merged (child overrides parent), elements taken from the child-most
// definition. Face.Texture entries are still "#name" refs — call
// ResolveTexture to turn one into a concrete location.
type Model struct {
	AmbientOcclusion bool
	Textures         map[string]string
	Elements         []Element
}

// ModelSource fetches raw model JSON by resource location
// (e.g. "minecraft:block/stone"). The client-jar Assets implements it.
type ModelSource interface {
	RawModel(loc string) (*RawModel, error)
}

// Resolve walks the parent chain of loc and flattens it into a Model.
//
// Merge rules mirror the vanilla loader:
//   - textures: accumulate root→leaf so the child-most value wins;
//   - ambientocclusion: nearest (child-most) explicit value wins, default true;
//   - elements: the child-most model that defines any elements replaces the
//     parent's entirely (elements are never merged).
func Resolve(src ModelSource, loc string) (*Model, error) {
	var chain []*RawModel
	seen := map[string]bool{}
	for cur := normalizeLoc(loc); cur != ""; {
		if seen[cur] {
			break // defensive: cyclic parent
		}
		seen[cur] = true
		rm, err := src.RawModel(cur)
		if err != nil {
			// builtin/* parents (items) have no JSON file; a block chain
			// terminates at block/block, so a lookup miss ends the walk.
			break
		}
		chain = append(chain, rm)
		cur = normalizeLoc(rm.Parent)
	}

	m := &Model{Textures: map[string]string{}, AmbientOcclusion: true}
	aoSet := false
	// root (last) → leaf (first): later writes win, so the leaf overrides.
	for i := len(chain) - 1; i >= 0; i-- {
		rm := chain[i]
		for k, v := range rm.Textures {
			m.Textures[k] = v
		}
		if rm.AmbientOcclusion != nil {
			m.AmbientOcclusion = *rm.AmbientOcclusion
			aoSet = true
		}
	}
	_ = aoSet
	// leaf → root: first model that defines elements wins.
	for _, rm := range chain {
		if len(rm.Elements) > 0 {
			m.Elements = rm.Elements
			break
		}
	}
	return m, nil
}

// ResolveTexture turns a face's texture reference into a concrete resource
// location, chasing texture variables transitively.
//
// Vanilla resolves a reference by looking its name up in the model's texture
// slots first — the leading '#' is only a convention. Some vanilla models
// (e.g. heavy_core) reference a slot from a face with a *bare* name ("all"),
// so a bare name that matches a slot is a reference to chase; a bare name that
// matches no slot is a literal texture location. A '#'-prefixed name with no
// slot is a dangling reference. Returns "" for a dangling '#'-ref or a cycle
// (the mesher substitutes the missing-texture sprite in that case).
func (m *Model) ResolveTexture(ref string) string {
	seen := map[string]bool{}
	for {
		if strings.HasPrefix(ref, "#") {
			name := ref[1:]
			if seen[name] {
				return "" // cyclic variable
			}
			seen[name] = true
			v, ok := m.Textures[name]
			if !ok {
				return "" // dangling slot reference
			}
			ref = v
			continue
		}
		if v, ok := m.Textures[ref]; ok {
			if seen[ref] {
				return "" // cyclic (bare) reference
			}
			seen[ref] = true
			ref = v
			continue
		}
		return normalizeLoc(ref)
	}
}

// normalizeLoc canonicalizes a resource location by defaulting the namespace
// to "minecraft" when none is given ("block/stone" → "minecraft:block/stone").
func normalizeLoc(s string) string {
	if s == "" {
		return ""
	}
	if strings.Contains(s, ":") {
		return s
	}
	return "minecraft:" + s
}
