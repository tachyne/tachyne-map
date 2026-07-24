package render

import (
	"encoding/json"
	"testing"
)

// fakeSource is an in-memory ModelSource for tests (no jar needed).
type fakeSource map[string]*RawModel

func (f fakeSource) RawModel(loc string) (*RawModel, error) {
	m, ok := f[loc]
	if !ok {
		return nil, errNotFound
	}
	return m, nil
}

var errNotFound = &notFound{}

type notFound struct{}

func (*notFound) Error() string { return "not found" }

func b(v bool) *bool { return &v }

func TestResolveParentChain(t *testing.T) {
	// block/block (root, elements) ← block/cube_all (texture vars) ← block/stone (binds #all).
	src := fakeSource{
		"minecraft:block/block": {
			AmbientOcclusion: b(true),
			Elements: []Element{{
				From: [3]float64{0, 0, 0}, To: [3]float64{16, 16, 16},
				Faces: map[string]Face{
					"up":    {Texture: "#all"},
					"north": {Texture: "#all", CullFace: "north"},
				},
			}},
		},
		"minecraft:block/cube_all": {
			Parent:   "block/block",
			Textures: map[string]string{"all": "#everything", "particle": "#all"},
		},
		"minecraft:block/stone": {
			Parent:   "minecraft:block/cube_all",
			Textures: map[string]string{"everything": "block/stone"},
		},
	}

	m, err := Resolve(src, "block/stone")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(m.Elements) != 1 {
		t.Fatalf("elements: got %d, want 1 (inherited from block/block)", len(m.Elements))
	}
	if !m.AmbientOcclusion {
		t.Errorf("ambientocclusion: got false, want true")
	}
	// #all → #everything → block/stone (transitive variable resolution).
	if got := m.ResolveTexture("#all"); got != "minecraft:block/stone" {
		t.Errorf("ResolveTexture(#all) = %q, want minecraft:block/stone", got)
	}
	// The face reference resolves the same way.
	if got := m.ResolveTexture(m.Elements[0].Faces["up"].Texture); got != "minecraft:block/stone" {
		t.Errorf("face up texture = %q, want minecraft:block/stone", got)
	}
	if got := m.ResolveTexture("#particle"); got != "minecraft:block/stone" {
		t.Errorf("ResolveTexture(#particle) = %q, want minecraft:block/stone", got)
	}
}

// TestResolveBareSlotReference covers vanilla models (heavy_core) whose faces
// reference a texture slot by bare name, without the '#'.
func TestResolveBareSlotReference(t *testing.T) {
	m := &Model{Textures: map[string]string{
		"all":      "block/heavy_core",
		"particle": "block/heavy_core",
	}}
	if got := m.ResolveTexture("all"); got != "minecraft:block/heavy_core" {
		t.Errorf("bare slot ref: got %q, want minecraft:block/heavy_core", got)
	}
	// A bare name that is NOT a slot is a literal location.
	if got := m.ResolveTexture("block/stone"); got != "minecraft:block/stone" {
		t.Errorf("bare literal: got %q, want minecraft:block/stone", got)
	}
}

func TestResolveTextureUnboundAndCyclic(t *testing.T) {
	m := &Model{Textures: map[string]string{
		"a": "#b",
		"b": "#a", // cycle
		"c": "#missing",
	}}
	if got := m.ResolveTexture("#a"); got != "" {
		t.Errorf("cyclic ref: got %q, want empty", got)
	}
	if got := m.ResolveTexture("#c"); got != "" {
		t.Errorf("unbound ref: got %q, want empty", got)
	}
	if got := m.ResolveTexture("block/dirt"); got != "minecraft:block/dirt" {
		t.Errorf("concrete ref: got %q, want minecraft:block/dirt", got)
	}
}

func TestVariantSelection(t *testing.T) {
	bs := mustBlockState(t, `{
		"variants": {
			"": {"model": "block/stone"}
		}
	}`)
	refs := bs.Chosen(map[string]string{"anything": "x"})
	if len(refs) != 1 || refs[0].Model != "minecraft:block/stone" {
		t.Fatalf("catch-all variant: got %+v", refs)
	}
}

func TestVariantMostSpecificWins(t *testing.T) {
	// A slab has type=top/bottom/double; the key must match on the exact value.
	bs := mustBlockState(t, `{
		"variants": {
			"type=bottom": {"model": "block/oak_slab"},
			"type=top":    {"model": "block/oak_slab_top", "y": 180},
			"type=double": {"model": "block/oak_planks"}
		}
	}`)
	refs := bs.Chosen(map[string]string{"type": "top"})
	if len(refs) != 1 || refs[0].Model != "minecraft:block/oak_slab_top" || refs[0].Y != 180 {
		t.Fatalf("type=top: got %+v", refs)
	}
	if refs := bs.Chosen(map[string]string{"type": "double"}); len(refs) != 1 ||
		refs[0].Model != "minecraft:block/oak_planks" {
		t.Fatalf("type=double: got %+v", refs)
	}
}

func TestVariantWeightedListTakesFirst(t *testing.T) {
	bs := mustBlockState(t, `{
		"variants": {
			"": [
				{"model": "block/grass_a", "weight": 3},
				{"model": "block/grass_b", "weight": 1}
			]
		}
	}`)
	refs := bs.Chosen(nil)
	if len(refs) != 1 || refs[0].Model != "minecraft:block/grass_a" {
		t.Fatalf("weighted list: got %+v", refs)
	}
}

func TestMultipartOverlaysMatchingParts(t *testing.T) {
	// A fence-like block: a post always, plus a side per connected direction.
	bs := mustBlockState(t, `{
		"multipart": [
			{"apply": {"model": "block/fence_post"}},
			{"when": {"north": "true"}, "apply": {"model": "block/fence_side", "y": 0}},
			{"when": {"east": "true"},  "apply": {"model": "block/fence_side", "y": 90}},
			{"when": {"OR": [{"north": "true"}, {"south": "true"}]}, "apply": {"model": "block/tall_marker"}}
		]
	}`)
	refs := bs.Chosen(map[string]string{"north": "true", "east": "false", "south": "false", "west": "false"})
	models := map[string]bool{}
	for _, r := range refs {
		models[r.Model] = true
	}
	if !models["minecraft:block/fence_post"] {
		t.Errorf("post always present; got %+v", refs)
	}
	if !models["minecraft:block/fence_side"] {
		t.Errorf("north side should apply; got %+v", refs)
	}
	if !models["minecraft:block/tall_marker"] {
		t.Errorf("OR(north,south) should match on north; got %+v", refs)
	}
	if models["minecraft:block/fence_side"] && countModel(refs, "minecraft:block/fence_side") != 1 {
		t.Errorf("east side should NOT apply (east=false); got %+v", refs)
	}
}

func TestPropsKeyStable(t *testing.T) {
	got := PropsKey(map[string]string{"half": "bottom", "facing": "north"})
	if got != "facing=north,half=bottom" {
		t.Errorf("PropsKey = %q, want facing=north,half=bottom", got)
	}
	if PropsKey(nil) != "" {
		t.Errorf("PropsKey(nil) should be empty")
	}
}

func mustBlockState(t *testing.T, s string) *RawBlockState {
	t.Helper()
	var bs RawBlockState
	if err := json.Unmarshal([]byte(s), &bs); err != nil {
		t.Fatalf("unmarshal blockstate: %v", err)
	}
	return &bs
}

func countModel(refs []ModelRef, model string) int {
	n := 0
	for _, r := range refs {
		if r.Model == model {
			n++
		}
	}
	return n
}
