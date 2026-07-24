package render

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math"
)

// Colormaps holds the vanilla grass/foliage colormap images. Grass and foliage
// block tints are sampled from these by a biome's (temperature, downfall);
// water and other tints are per-biome constants handled elsewhere.
type Colormaps struct {
	grass, foliage *image.RGBA
}

// LoadColormaps reads the grass and foliage colormaps from the client jar.
func LoadColormaps(a *Assets) (*Colormaps, error) {
	grass, err := readColormap(a, "minecraft:colormap/grass")
	if err != nil {
		return nil, err
	}
	foliage, err := readColormap(a, "minecraft:colormap/foliage")
	if err != nil {
		return nil, err
	}
	return &Colormaps{grass: grass, foliage: foliage}, nil
}

func readColormap(a *Assets, loc string) (*image.RGBA, error) {
	b, err := a.Texture(loc)
	if err != nil {
		return nil, err
	}
	img, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	rgba := image.NewRGBA(img.Bounds())
	for y := img.Bounds().Min.Y; y < img.Bounds().Max.Y; y++ {
		for x := img.Bounds().Min.X; x < img.Bounds().Max.X; x++ {
			rgba.Set(x, y, img.At(x, y))
		}
	}
	return rgba, nil
}

// Grass returns the grass tint for a biome's temperature and downfall,
// following vanilla's GrassColor.get sampling of the 256×256 colormap.
func (c *Colormaps) Grass(temperature, downfall float64) color.RGBA {
	return sampleColormap(c.grass, temperature, downfall)
}

// Foliage returns the foliage (leaves) tint for a biome's temperature and
// downfall.
func (c *Colormaps) Foliage(temperature, downfall float64) color.RGBA {
	return sampleColormap(c.foliage, temperature, downfall)
}

// sampleColormap mirrors vanilla: clamp temperature/downfall to [0,1], scale
// downfall by temperature, then index the colormap at
// x=(1-temp)*255, y=(1-downfall*temp)*255.
func sampleColormap(m *image.RGBA, temperature, downfall float64) color.RGBA {
	t := clamp01(temperature)
	d := clamp01(downfall) * t
	x := int((1.0 - t) * 255.0)
	y := int((1.0 - d) * 255.0)
	b := m.Bounds()
	x = clampInt(x, 0, b.Dx()-1)
	y = clampInt(y, 0, b.Dy()-1)
	r, g, bl, _ := m.At(b.Min.X+x, b.Min.Y+y).RGBA()
	return color.RGBA{uint8(r >> 8), uint8(g >> 8), uint8(bl >> 8), 255}
}

func clamp01(v float64) float64 { return math.Max(0, math.Min(1, v)) }

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
