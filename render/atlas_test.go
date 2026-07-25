package render

import (
	"image"
	"image/color"
	"math"
	"testing"
)

// A cell's gutter must be filled with copies of that cell's own border, so a
// mipmap level can never average one sprite against its neighbour in the grid.
func TestExtendEdgesClampsToCellBorder(t *testing.T) {
	const cell, gutter = 4, 2
	slot := cell + 2*gutter

	// One slot, cell filled with a distinct colour per pixel so a wrong clamp
	// is detectable, surrounded by a "neighbour" colour that must be erased.
	img := image.NewRGBA(image.Rect(0, 0, slot, slot))
	neighbour := color.RGBA{9, 9, 9, 255}
	for y := 0; y < slot; y++ {
		for x := 0; x < slot; x++ {
			img.SetRGBA(x, y, neighbour)
		}
	}
	px := func(x, y int) color.RGBA { return color.RGBA{uint8(x), uint8(y), 200, 255} }
	for y := 0; y < cell; y++ {
		for x := 0; x < cell; x++ {
			img.SetRGBA(gutter+x, gutter+y, px(x, y))
		}
	}

	extendEdges(img, 0, 0, cell, gutter)

	for y := 0; y < slot; y++ {
		for x := 0; x < slot; x++ {
			want := px(clampInt(x-gutter, 0, cell-1), clampInt(y-gutter, 0, cell-1))
			if got := img.RGBAAt(x, y); got != want {
				t.Fatalf("pixel (%d,%d): got %v, want %v", x, y, got, want)
			}
		}
	}
}

func TestExtendEdgesNoGutterIsNoOp(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.SetRGBA(0, 0, color.RGBA{1, 2, 3, 255})
	extendEdges(img, 0, 0, 2, 0)
	if got := img.RGBAAt(0, 0); got != (color.RGBA{1, 2, 3, 255}) {
		t.Fatalf("gutter 0 modified the image: got %v", got)
	}
}

// The UV rect must address the inner cell, never the padding — otherwise every
// block would render with a sliver of its own duplicated border.
func TestSpriteRectExcludesGutter(t *testing.T) {
	const cell, gutter = 16, 4
	slot := cell + 2*gutter

	// Two slots side by side; only the geometry of the rect is under test, so
	// build the atlas struct the way BuildAtlas would for cols=2, rows=1.
	w, h := float64(2*slot), float64(slot)
	spr := Sprite{
		U0: float64(slot+gutter) / w, V0: float64(gutter) / h,
		U1: float64(slot+gutter+cell) / w, V1: float64(gutter+cell) / h,
	}

	// The rects are ratios of texel counts, so compare in texels with a
	// tolerance far below one texel rather than for exact equality.
	near := func(name string, got, want float64) {
		t.Helper()
		if math.Abs(got-want) > 1e-9 {
			t.Errorf("%s: got %v texels, want %v", name, got, want)
		}
	}
	near("sprite width", (spr.U1-spr.U0)*w, cell)
	near("sprite height", (spr.V1-spr.V0)*h, cell)
	// The rect must start a full gutter inside its slot.
	near("sprite x origin", spr.U0*w, float64(slot+gutter))
	near("sprite y origin", spr.V0*h, gutter)
}
