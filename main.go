// Command tachyne-map is the map pod: it opens a tachyne world read-only,
// renders its chunks into WebGL tiles natively in Go (no Java, no Anvil), and
// serves the tiles plus the embedded three.js viewer over HTTP.
//
// The world is a pure function of (seed, edits), so the pod needs only the seed
// to reproduce terrain — it meshes tiles on demand and caches them. Player
// edits and live updates arrive later (M4) over the NATS bus; today the pod
// renders terrain from the seed.
package main

import (
	"bytes"
	"embed"
	"encoding/json"
	"flag"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/tachyne/tachyne-map/render"
	"github.com/tachyne/tachyne-world/worldread"
)

//go:embed web/index.html web/app.js web/vendor
var webFS embed.FS

func main() {
	addr := flag.String("addr", envOr("MAP_ADDR", ":8100"), "HTTP listen address")
	seed := flag.Int64("seed", envInt64("MAP_SEED", 1), "world seed (cluster classic world = 1)")
	version := flag.String("version", envOr("MAP_VERSION", "1.21.11"), "Minecraft asset version")
	cacheDir := flag.String("cache", envOr("MAP_CACHE", "/var/cache/tachyne-map"), "asset cache dir")
	accept := flag.Bool("accept-download", envBool("MAP_ACCEPT_DOWNLOAD", false),
		"assert acceptance of Mojang's EULA to fetch the client jar")
	radius := flag.Int("radius", envInt("MAP_RADIUS", 8), "chunk radius served around the center")
	cx := flag.Int("cx", envInt("MAP_CX", 0), "center chunk X")
	cz := flag.Int("cz", envInt("MAP_CZ", 0), "center chunk Z")
	flag.Parse()

	log.Printf("tachyne-map: provisioning %s assets into %s", *version, *cacheDir)
	jar, err := render.EnsureClientJar(*cacheDir, *version, *accept)
	if err != nil {
		log.Fatalf("client jar: %v", err)
	}
	assets, err := render.OpenJar(jar)
	if err != nil {
		log.Fatalf("open jar: %v", err)
	}
	locs, err := render.ReferencedBlockTextures(assets)
	if err != nil {
		log.Fatalf("scan textures: %v", err)
	}
	locs = append(locs, render.FluidTextures...)
	atlas := render.BuildAtlas(assets, locs, 16)
	cm, err := render.LoadColormaps(assets)
	if err != nil {
		log.Fatalf("colormaps: %v", err)
	}
	mesher := render.NewMesher(assets, atlas, cm)

	reader, err := worldread.Open(worldread.Overworld, *seed, nil) // terrain only (edits: M4)
	if err != nil {
		log.Fatalf("open world: %v", err)
	}

	var atlasBuf bytes.Buffer
	if err := atlas.EncodePNG(&atlasBuf); err != nil {
		log.Fatalf("encode atlas: %v", err)
	}

	srv := &server{
		mesher:   mesher,
		reader:   reader,
		atlasPNG: atlasBuf.Bytes(),
		cache:    map[[2]int]*render.Tile{},
		dim:      reader.Dim().String(),
		live:     newLiveHub(),
	}
	srv.manifest = srv.buildManifest(*cx, *cz, *radius)
	go srv.runFlusher()

	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Fatalf("web fs: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/manifest.json", srv.handleManifest)
	mux.HandleFunc("/atlas.png", srv.handleAtlas)
	mux.HandleFunc("/tile/", srv.handleTile)
	mux.HandleFunc("/events", srv.handleEvents)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.Handle("/", http.FileServer(http.FS(sub)))

	log.Printf("tachyne-map: serving %s on %s (seed %d, region ±%d chunks around %d,%d)",
		srv.dim, *addr, *seed, *radius, *cx, *cz)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		log.Fatal(err)
	}
}

type server struct {
	mesher   *render.Mesher
	reader   *worldread.Reader
	atlasPNG []byte
	manifest []byte
	dim      string

	live *liveHub // tile invalidation + SSE fan-out

	mu    sync.Mutex
	cache map[[2]int]*render.Tile
	order [][2]int // insertion order, for bounded eviction
}

// maxCachedTiles bounds the meshed-tile cache so the streaming viewer panning
// across the world can't grow pod memory without limit (re-meshing is ~15ms).
//
// Sized deliberately small: a tile is ~0.5 MB of geometry, and the pod ALSO
// carries the world's own byte-budgeted caches (generated chunks ~256 MB +
// light). At 1500 this cache alone was ~750 MB and panning into fresh terrain
// — which grows all three at once — OOM-killed the pod. The viewer keeps its
// own resident ring (~289 tiles), so this only needs to cover revisits.
const maxCachedTiles = 256

// buildManifest advertises the served region: the tile grid plus spawn/atlas
// metadata the viewer needs.
func (s *server) buildManifest(cx, cz, radius int) []byte {
	ch := s.reader.Chunk(cx, cz)
	surfaceY := int(ch.Heightmap[8*16+8]) + 2
	var tiles [][2]int
	for x := cx - radius; x <= cx+radius; x++ {
		for z := cz - radius; z <= cz+radius; z++ {
			tiles = append(tiles, [2]int{x, z})
		}
	}
	m := map[string]any{
		"name":      "tachyne",
		"dim":       s.dim,
		"spawn":     []float64{float64(cx*16 + 8), float64(surfaceY), float64(cz*16 + 8)},
		"atlasCell": 16,
		"tiles":     tiles,
	}
	b, _ := json.Marshal(m)
	return b
}

func (s *server) handleManifest(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write(s.manifest)
}

func (s *server) handleAtlas(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Write(s.atlasPNG)
}

// handleTile meshes /tile/<dim>/<cx>/<cz>.json on demand (cached), gzipping when
// the client accepts it.
func (s *server) handleTile(w http.ResponseWriter, r *http.Request) {
	// /tile/overworld/<cx>/<cz>.json
	rest := strings.TrimPrefix(r.URL.Path, "/tile/")
	parts := strings.Split(rest, "/")
	if len(parts) != 3 {
		http.NotFound(w, r)
		return
	}
	dim := parts[0]
	cxStr := parts[1]
	czStr := strings.TrimSuffix(parts[2], ".json")
	cx, err1 := strconv.Atoi(cxStr)
	cz, err2 := strconv.Atoi(czStr)
	if dim != s.dim || err1 != nil || err2 != nil {
		http.NotFound(w, r)
		return
	}

	tile := s.tile(cx, cz)
	w.Header().Set("Content-Type", "application/json")
	if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		w.Header().Set("Content-Encoding", "gzip")
		tile.EncodeGZ(w)
		return
	}
	tile.EncodeJSON(w)
}

// tile returns the meshed tile for (cx,cz), meshing and caching on first request.
func (s *server) tile(cx, cz int) *render.Tile {
	key := [2]int{cx, cz}
	s.mu.Lock()
	if t, ok := s.cache[key]; ok {
		s.mu.Unlock()
		return t
	}
	s.mu.Unlock()

	t := s.mesher.MeshChunk(s.reader, cx, cz)

	s.mu.Lock()
	if _, ok := s.cache[key]; !ok {
		s.cache[key] = t
		s.order = append(s.order, key)
		if len(s.order) > maxCachedTiles {
			n := maxCachedTiles / 5 // evict the oldest fifth
			for _, k := range s.order[:n] {
				delete(s.cache, k)
			}
			s.order = append(s.order[:0], s.order[n:]...)
		}
	}
	s.mu.Unlock()
	return t
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envInt64(key string, def int64) int64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}

func envBool(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		return v == "1" || strings.EqualFold(v, "true")
	}
	return def
}
