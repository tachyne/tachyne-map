package render

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"math"
	"sort"
)

// Sprite is one texture's placement in the atlas as a UV rect in [0,1], with V
// measured from the top of the image (Minecraft's model-UV convention).
type Sprite struct {
	U0, V0, U1, V1 float64
}

// DefaultGutter is the edge-extended padding placed around every atlas cell,
// as a fraction of the cell edge. Four texels at vanilla's 16px cell leaves the
// first three mipmap levels (16→8→4) sampling a sprite only against copies of
// its own border, which covers every minification the viewer actually reaches:
// at the streaming radius a block is still ~4 screen pixels.
const DefaultGutter = 4

// Atlas is a grid of block textures stitched into one image, with the UV rect
// of each source texture. The mesher maps a face's 0..16 model UV into the
// sprite's rect to get final atlas coordinates.
//
// Each cell sits in a larger slot, padded by Gutter texels of its own
// edge-extended border. Without that padding the atlas cannot be mipmapped —
// coarse levels would average neighbouring sprites together — and without
// mipmaps distant terrain aliases badly, because one screen pixel then samples
// a single arbitrary texel out of the 256 it covers.
type Atlas struct {
	Img     *image.RGBA
	Sprites map[string]Sprite // texture location -> UV rect
	Cell    int               // cell edge, pixels
	Gutter  int               // edge-extended padding around each cell, pixels
	Missing Sprite            // fallback rect for textures that failed to load
}

// BuildAtlas stitches the given texture locations into a square-ish grid atlas
// at cell×cell pixels each (cell<=0 defaults to 16, vanilla's base size),
// padded by gutter texels of edge extension (gutter<0 defaults to
// DefaultGutter; 0 disables padding and makes the atlas unsafe to mipmap).
// Animated textures contribute their first frame; non-cell sizes are
// nearest-neighbor scaled so the grid stays uniform and pixel-art crisp. A
// synthetic magenta "missing" cell is always slot 0 and is used for any
// texture that fails to load.
func BuildAtlas(a *Assets, locs []string, cell, gutter int) *Atlas {
	if cell <= 0 {
		cell = 16
	}
	if gutter < 0 {
		gutter = DefaultGutter
	}
	slot := cell + 2*gutter
	uniq := dedupeSorted(locs)

	// Slot 0 is the missing-texture sprite; real textures follow.
	n := len(uniq) + 1
	cols := int(math.Ceil(math.Sqrt(float64(n))))
	rows := (n + cols - 1) / cols
	at := &Atlas{
		Img:     image.NewRGBA(image.Rect(0, 0, cols*slot, rows*slot)),
		Sprites: make(map[string]Sprite, len(uniq)),
		Cell:    cell,
		Gutter:  gutter,
	}

	place := func(idx int, src *image.RGBA) Sprite {
		sx, sy := (idx%cols)*slot, (idx/cols)*slot
		cx, cy := sx+gutter, sy+gutter
		draw.Draw(at.Img, image.Rect(cx, cy, cx+cell, cy+cell), src, image.Point{}, draw.Src)
		extendEdges(at.Img, sx, sy, cell, gutter)
		w, h := float64(at.Img.Bounds().Dx()), float64(at.Img.Bounds().Dy())
		return Sprite{
			U0: float64(cx) / w, V0: float64(cy) / h,
			U1: float64(cx+cell) / w, V1: float64(cy+cell) / h,
		}
	}

	at.Missing = place(0, missingCell(cell))
	for i, loc := range uniq {
		spr, err := loadSprite(a, loc, cell)
		if err != nil {
			at.Sprites[loc] = at.Missing
			continue
		}
		at.Sprites[loc] = place(i+1, spr)
	}
	return at
}

// Lookup returns the sprite for a texture location, or the missing sprite.
func (at *Atlas) Lookup(loc string) Sprite {
	if s, ok := at.Sprites[loc]; ok {
		return s
	}
	return at.Missing
}

// EncodePNG writes the atlas image as PNG.
func (at *Atlas) EncodePNG(w io.Writer) error { return png.Encode(w, at.Img) }

// loadSprite loads a texture's first frame as a cell×cell RGBA image.
func loadSprite(a *Assets, loc string, cell int) (*image.RGBA, error) {
	b, err := a.Texture(loc)
	if err != nil {
		return nil, err
	}
	img, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	bnd := img.Bounds()
	side := bnd.Dx()
	if bnd.Dy() < side {
		side = bnd.Dy() // defensive; block textures are width-aligned
	}
	// First animation frame = the top-left side×side square (frames stack
	// vertically).
	frame := image.NewRGBA(image.Rect(0, 0, side, side))
	draw.Draw(frame, frame.Bounds(), img, bnd.Min, draw.Src)
	if side == cell {
		return frame, nil
	}
	return nearestScale(frame, cell), nil
}

// extendEdges fills the gutter ring of the slot at (slotX,slotY) by clamping to
// the nearest pixel of the cell it surrounds. Every texel a mipmap level can
// reach outside a sprite is then a copy of that sprite's own border rather than
// its neighbour in the grid, so coarser levels stay free of colour bleed.
func extendEdges(img *image.RGBA, slotX, slotY, cell, gutter int) {
	if gutter <= 0 {
		return
	}
	slot := cell + 2*gutter
	for y := 0; y < slot; y++ {
		for x := 0; x < slot; x++ {
			if x >= gutter && x < gutter+cell && y >= gutter && y < gutter+cell {
				continue // the cell itself, already drawn
			}
			src := image.Point{
				X: slotX + gutter + clampInt(x-gutter, 0, cell-1),
				Y: slotY + gutter + clampInt(y-gutter, 0, cell-1),
			}
			img.SetRGBA(slotX+x, slotY+y, img.RGBAAt(src.X, src.Y))
		}
	}
}

// nearestScale resamples src to size×size with nearest-neighbor (keeps the
// pixel-art look; no blurring).
func nearestScale(src *image.RGBA, size int) *image.RGBA {
	dst := image.NewRGBA(image.Rect(0, 0, size, size))
	sw, sh := src.Bounds().Dx(), src.Bounds().Dy()
	for y := 0; y < size; y++ {
		sy := y * sh / size
		for x := 0; x < size; x++ {
			sx := x * sw / size
			dst.SetRGBA(x, y, src.RGBAAt(src.Bounds().Min.X+sx, src.Bounds().Min.Y+sy))
		}
	}
	return dst
}

// missingCell is a magenta/black 2×2 checker scaled to cell — the classic
// missing-texture sprite.
func missingCell(cell int) *image.RGBA {
	magenta := color.RGBA{248, 0, 248, 255}
	black := color.RGBA{0, 0, 0, 255}
	base := image.NewRGBA(image.Rect(0, 0, 2, 2))
	base.SetRGBA(0, 0, magenta)
	base.SetRGBA(1, 1, magenta)
	base.SetRGBA(1, 0, black)
	base.SetRGBA(0, 1, black)
	return nearestScale(base, cell)
}

func dedupeSorted(xs []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		if x != "" && !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	sort.Strings(out)
	return out
}

// ReferencedBlockTextures returns every texture location that any block can
// render, by resolving each blockstate's models and their face textures. This
// is exactly the set the atlas must contain for the mesher.
func ReferencedBlockTextures(a *Assets) ([]string, error) {
	set := map[string]bool{}
	for _, name := range a.BlockStateNames() {
		bs, err := a.BlockState(name)
		if err != nil {
			continue
		}
		for _, ref := range bs.AllRefs() {
			m, err := Resolve(a, ref.Model)
			if err != nil {
				continue
			}
			for _, el := range m.Elements {
				for _, face := range el.Faces {
					if loc := m.ResolveTexture(face.Texture); loc != "" {
						set[loc] = true
					}
				}
			}
		}
	}
	out := make([]string, 0, len(set))
	for loc := range set {
		out = append(out, loc)
	}
	sort.Strings(out)
	return out, nil
}
