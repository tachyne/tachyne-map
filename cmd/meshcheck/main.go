// meshcheck runs the full render pipeline against the real world: provision the
// client jar, build the atlas + colormaps, open the world read-only from a
// seed, and mesh a square region of chunks. It reports geometry stats and,
// with -out, writes viewer-ready data (atlas.png + manifest.json + tiles) so
// the three.js viewer can render it (serve web/ and open ?data=<out>).
//
//	go run ./cmd/meshcheck -seed 1 -cx 0 -cz 0 -radius 4 -out web/preview
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"image"
	"image/png"
	"log"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/tachyne/tachyne-map/render"
	"github.com/tachyne/tachyne-world/worldread"
)

func main() {
	version := flag.String("version", "1.21.11", "Minecraft version for assets")
	cache := flag.String("cache", defaultCache(), "asset cache dir")
	seed := flag.Int64("seed", 1, "world seed (cluster classic world = 1)")
	cx := flag.Int("cx", 0, "center chunk X")
	cz := flag.Int("cz", 0, "center chunk Z")
	radius := flag.Int("radius", 4, "chunk radius to mesh around center")
	out := flag.String("out", "", "if set, write viewer data (atlas+manifest+tiles) here")
	png := flag.String("png", "", "if set, write a top-down flat-shaded preview PNG here")
	surfaceDepth := flag.Int("surface-depth", 0, "skip geometry buried more than N blocks below its column surface (0 = keep everything)")
	flag.Parse()

	jar, err := render.EnsureClientJar(*cache, *version, true)
	if err != nil {
		log.Fatalf("client jar: %v", err)
	}
	a, err := render.OpenJar(jar)
	if err != nil {
		log.Fatalf("open jar: %v", err)
	}
	defer a.Close()

	locs, _ := render.ReferencedBlockTextures(a)
	locs = append(locs, render.FluidTextures...)
	atlas := render.BuildAtlas(a, locs, 16, render.DefaultGutter)
	cm, err := render.LoadColormaps(a)
	if err != nil {
		log.Fatalf("colormaps: %v", err)
	}
	mesher := render.NewMesher(a, atlas, cm)
	mesher.SurfaceDepth = *surfaceDepth

	r, err := worldread.Open(worldread.Overworld, *seed, nil) // terrain only
	if err != nil {
		log.Fatalf("open world: %v", err)
	}
	fmt.Printf("seed %d, atlas %d sprites (%dx%d px), meshing %d chunks around (%d,%d)\n",
		*seed, len(atlas.Sprites), atlas.Img.Bounds().Dx(), atlas.Img.Bounds().Dy(),
		(2*(*radius)+1)*(2*(*radius)+1), *cx, *cz)

	var tiles []*render.Tile
	var totalVerts, totalTris int
	start := time.Now()
	for x := *cx - *radius; x <= *cx+*radius; x++ {
		for z := *cz - *radius; z <= *cz+*radius; z++ {
			t := mesher.MeshChunk(r, x, z)
			if t.Empty() {
				continue
			}
			totalVerts += t.VertexCount()
			totalTris += len(t.Indices) / 3
			tiles = append(tiles, t)
		}
	}
	dur := time.Since(start)

	fmt.Printf("\n== mesh results ==\n")
	fmt.Printf("non-empty tiles : %d\n", len(tiles))
	fmt.Printf("vertices        : %d\n", totalVerts)
	fmt.Printf("triangles       : %d\n", totalTris)
	fmt.Printf("mesh time        : %s (%.1f ms/chunk)\n", dur.Round(time.Millisecond),
		float64(dur.Milliseconds())/float64((2*(*radius)+1)*(2*(*radius)+1)))

	// Sanity: index bounds + attribute sizes on a sample tile.
	if len(tiles) > 0 {
		t := tiles[0]
		vc := uint32(t.VertexCount())
		bad := 0
		for _, idx := range t.Indices {
			if idx >= vc {
				bad++
			}
		}
		fmt.Printf("sample tile (%d,%d): %d verts, %d tris, %d out-of-range indices\n",
			t.CX, t.CZ, t.VertexCount(), len(t.Indices)/3, bad)
	}

	if *out != "" {
		if err := writeViewerData(*out, atlas, r, tiles, *cx, *cz); err != nil {
			log.Fatalf("write viewer data: %v", err)
		}
		fmt.Printf("\nwrote viewer data -> %s (serve web/ and open ?data=%s)\n", *out, filepath.Base(*out))
	}

	if *png != "" {
		if err := renderTopDown(tiles, *png, 6); err != nil {
			log.Fatalf("render png: %v", err)
		}
		fmt.Printf("wrote top-down preview -> %s\n", *png)
	}
}

// renderTopDown rasterizes the meshed tiles into a flat-shaded top-down PNG:
// each triangle is filled with its averaged baked color (light+tint+shade),
// depth-tested by world-Y so the highest surface wins. No texturing — just
// enough to confirm terrain shape, water, and tint look right.
func renderTopDown(tiles []*render.Tile, path string, px float64) error {
	minX, minZ := math.Inf(1), math.Inf(1)
	maxX, maxZ := math.Inf(-1), math.Inf(-1)
	for _, t := range tiles {
		for i := 0; i < len(t.Positions); i += 3 {
			x, z := float64(t.Positions[i]), float64(t.Positions[i+2])
			minX, maxX = math.Min(minX, x), math.Max(maxX, x)
			minZ, maxZ = math.Min(minZ, z), math.Max(maxZ, z)
		}
	}
	W := int((maxX-minX)*px) + 1
	H := int((maxZ-minZ)*px) + 1
	if W <= 0 || H <= 0 {
		return fmt.Errorf("empty bounds")
	}
	img := image.NewRGBA(image.Rect(0, 0, W, H))
	for i := range img.Pix {
		img.Pix[i] = 0
	}
	zbuf := make([]float64, W*H)
	for i := range zbuf {
		zbuf[i] = math.Inf(-1)
	}

	for _, t := range tiles {
		for k := 0; k < len(t.Indices); k += 3 {
			i0, i1, i2 := t.Indices[k], t.Indices[k+1], t.Indices[k+2]
			p := func(idx uint32) (float64, float64, float64, [3]float64) {
				b := idx * 3
				return float64(t.Positions[b]), float64(t.Positions[b+1]), float64(t.Positions[b+2]),
					[3]float64{float64(t.Colors[b]), float64(t.Colors[b+1]), float64(t.Colors[b+2])}
			}
			x0, y0, z0, c0 := p(i0)
			x1, y1, z1, c1 := p(i1)
			x2, y2, z2, c2 := p(i2)
			depth := (y0 + y1 + y2) / 3
			col := [3]float64{(c0[0] + c1[0] + c2[0]) / 3, (c0[1] + c1[1] + c2[1]) / 3, (c0[2] + c1[2] + c2[2]) / 3}
			ax, ay := (x0-minX)*px, (z0-minZ)*px
			bx, by := (x1-minX)*px, (z1-minZ)*px
			cx2, cy := (x2-minX)*px, (z2-minZ)*px
			fillTri(img, zbuf, W, H, ax, ay, bx, by, cx2, cy, depth, col)
		}
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, img)
}

func fillTri(img *image.RGBA, zbuf []float64, W, H int, ax, ay, bx, by, cx, cy, depth float64, col [3]float64) {
	minx := int(math.Floor(math.Min(ax, math.Min(bx, cx))))
	maxx := int(math.Ceil(math.Max(ax, math.Max(bx, cx))))
	miny := int(math.Floor(math.Min(ay, math.Min(by, cy))))
	maxy := int(math.Ceil(math.Max(ay, math.Max(by, cy))))
	if minx < 0 {
		minx = 0
	}
	if miny < 0 {
		miny = 0
	}
	if maxx >= W {
		maxx = W - 1
	}
	if maxy >= H {
		maxy = H - 1
	}
	area := edge(ax, ay, bx, by, cx, cy)
	if area == 0 {
		return
	}
	r := uint8(clamp01f(col[0]) * 255)
	g := uint8(clamp01f(col[1]) * 255)
	bl := uint8(clamp01f(col[2]) * 255)
	for y := miny; y <= maxy; y++ {
		for x := minx; x <= maxx; x++ {
			fx, fy := float64(x)+0.5, float64(y)+0.5
			w0 := edge(bx, by, cx, cy, fx, fy)
			w1 := edge(cx, cy, ax, ay, fx, fy)
			w2 := edge(ax, ay, bx, by, fx, fy)
			if (w0 >= 0 && w1 >= 0 && w2 >= 0) || (w0 <= 0 && w1 <= 0 && w2 <= 0) {
				idx := y*W + x
				if depth > zbuf[idx] {
					zbuf[idx] = depth
					o := idx * 4
					img.Pix[o], img.Pix[o+1], img.Pix[o+2], img.Pix[o+3] = r, g, bl, 255
				}
			}
		}
	}
}

func edge(ax, ay, bx, by, cx, cy float64) float64 {
	return (bx-ax)*(cy-ay) - (by-ay)*(cx-ax)
}

func clamp01f(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func writeViewerData(out string, atlas *render.Atlas, r *worldread.Reader, tiles []*render.Tile, cx, cz int) error {
	if err := os.MkdirAll(filepath.Join(out, "tile", "overworld"), 0o755); err != nil {
		return err
	}
	// atlas.png
	af, err := os.Create(filepath.Join(out, "atlas.png"))
	if err != nil {
		return err
	}
	if err := atlas.EncodePNG(af); err != nil {
		af.Close()
		return err
	}
	af.Close()

	// tiles + coord list
	var coords [][2]int
	for _, t := range tiles {
		dir := filepath.Join(out, "tile", "overworld", fmt.Sprint(t.CX))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		tf, err := os.Create(filepath.Join(dir, fmt.Sprintf("%d.json", t.CZ)))
		if err != nil {
			return err
		}
		if err := t.EncodeJSON(tf); err != nil {
			tf.Close()
			return err
		}
		tf.Close()
		coords = append(coords, [2]int{t.CX, t.CZ})
	}

	// manifest with spawn at the center column surface
	ch := r.Chunk(cx, cz)
	surfaceY := int(ch.Heightmap[8*16+8]) + 2
	manifest := map[string]any{
		"name":      "tachyne",
		"dim":       "overworld",
		"spawn":     []float64{float64(cx*16 + 8), float64(surfaceY), float64(cz*16 + 8)},
		"atlasCell": atlas.Cell,
		"tiles":     coords,
	}
	mf, err := os.Create(filepath.Join(out, "manifest.json"))
	if err != nil {
		return err
	}
	defer mf.Close()
	return json.NewEncoder(mf).Encode(manifest)
}

func defaultCache() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cache", "tachyne-map")
}
