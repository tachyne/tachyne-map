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

// Atlas is a grid of block textures stitched into one image, with the UV rect
// of each source texture. The mesher maps a face's 0..16 model UV into the
// sprite's rect to get final atlas coordinates.
type Atlas struct {
	Img     *image.RGBA
	Sprites map[string]Sprite // texture location -> UV rect
	Cell    int               // cell edge, pixels
	Missing Sprite            // fallback rect for textures that failed to load
}

// BuildAtlas stitches the given texture locations into a square-ish grid atlas
// at cell×cell pixels each (cell<=0 defaults to 16, vanilla's base size).
// Animated textures contribute their first frame; non-cell sizes are
// nearest-neighbor scaled so the grid stays uniform and pixel-art crisp. A
// synthetic magenta "missing" cell is always slot 0 and is used for any
// texture that fails to load.
func BuildAtlas(a *Assets, locs []string, cell int) *Atlas {
	if cell <= 0 {
		cell = 16
	}
	uniq := dedupeSorted(locs)

	// Slot 0 is the missing-texture sprite; real textures follow.
	n := len(uniq) + 1
	cols := int(math.Ceil(math.Sqrt(float64(n))))
	rows := (n + cols - 1) / cols
	at := &Atlas{
		Img:     image.NewRGBA(image.Rect(0, 0, cols*cell, rows*cell)),
		Sprites: make(map[string]Sprite, len(uniq)),
		Cell:    cell,
	}

	place := func(idx int, src *image.RGBA) Sprite {
		cx, cy := (idx%cols)*cell, (idx/cols)*cell
		draw.Draw(at.Img, image.Rect(cx, cy, cx+cell, cy+cell), src, image.Point{}, draw.Src)
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
