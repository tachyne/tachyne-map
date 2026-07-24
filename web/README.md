# tachyne-map web viewer

A self-contained browser 3D viewer for pre-meshed world tiles. The Go pod
serves this directory as static files plus the data endpoints below; the
viewer fetches the manifest, the texture atlas, and every listed tile, and
renders one `THREE.BufferGeometry` mesh per tile. All lighting and biome tint
is baked into per-vertex colors by the server-side mesher, so the scene has no
lights — a single unlit material multiplies the atlas texture by the baked
vertex color.

## Files

| Path | What |
|---|---|
| `index.html` | Page shell: canvas, HUD, import map for the bare `three` specifier. |
| `app.js` | Viewer logic (ES module): manifest/atlas/tile loading, mesh building, camera. |
| `vendor/three.module.js` | three.js **r170** module build (pinned, vendored). |
| `vendor/OrbitControls.js` | OrbitControls from the same release. |
| `vendor/THREE-LICENSE` | three.js MIT license text. |
| `NOTICE` | Third-party attribution. |
| `testdata/` | Synthetic fixtures for development without the real server. |

## Server contract

All paths are relative to the data base (default: wherever the page is served
from). Responses may be gzip-encoded on the wire; `fetch()` decodes
transparently.

### `GET /manifest.json`

```json
{
  "name": "world name",
  "dim": "overworld",
  "spawn": [x, y, z],          // floats, world coords
  "atlasCell": 16,             // px per atlas cell
  "tiles": [[cx, cz], ...]     // every available tile
}
```

### `GET /atlas.png`

The block-texture atlas: a square grid of `atlasCell`-pixel cells (the sample
is 528×528 = 33×33 cells). Sampled with nearest filtering, no mipmaps.

### `GET /tile/{dim}/{cx}/{cz}.json`

```json
{
  "dim": "overworld", "cx": 0, "cz": 0,
  "positions": [x, y, z, ...],  // world-space floats, 3 per vertex
  "uvs":       [u, v, ...],     // atlas coords 0..1, V FROM THE TOP of the image
  "colors":    [r, g, b, ...],  // 0..1, baked lighting + tint already applied
  "indices":   [...]            // uint triangle indices, CCW front faces
}
```

Contract points the renderer depends on:

- **V from the top**: the atlas texture is uploaded with `flipY = false`, so
  `v = 0` samples the top row of the PNG. Meshers must emit V measured from
  the image top (a side face's upper edge gets the *smaller* v).
- **CCW front faces**: the material is `side: THREE.FrontSide`; back faces are
  culled.
- **Baked light in colors**: material is `MeshBasicMaterial` with
  `vertexColors: true` — vertex RGB multiplies the texel, nothing else lights
  the scene.
- **`alphaTest: 0.5`**: cutout transparency (foliage etc.) works; smooth alpha
  does not — bake translucency some other way if it's ever needed.

## Load flow

1. `GET manifest.json` → camera is placed above/behind `spawn`, looking at it.
2. `GET atlas.png` → `NearestFilter` mag+min, `generateMipmaps = false`,
   `flipY = false`, sRGB color space.
3. All tiles in the manifest are fetched **nearest-to-spawn first**, at most 8
   in flight, and added to the scene as they arrive. A failed tile is logged
   and counted in the HUD; it never aborts the rest.

HUD (top-left) shows loaded/total tile count and camera coordinates; a
top-center banner shows load progress and errors. Mouse: left-drag orbits,
right-drag pans, wheel zooms (OrbitControls with damping).

## Local development (no server)

`testdata/` mirrors the contract with hand-built fixtures: the real 528×528
sample atlas plus three 16×16 floor tiles at chunk (0,0), (1,0) and (-1,0) —
tile (0,0) also carries a 4-block tower and a small arch so height, face
shading, and side-face V-orientation are visually checkable. Interior faces
between touching cubes are culled, exactly like a real mesher would.

```sh
cd web
python3 -m http.server 8080
# open http://localhost:8080/?data=testdata
```

The `?data=` query parameter rebases every data fetch (`manifest.json`,
`atlas.png`, `tile/...`) onto the given path or URL; without it the viewer
uses the page's own origin/path — the production layout, where the Go pod
serves both the static files and the data.

The fixtures were generated (and their winding/UV orientation mechanically
verified) by a throwaway script; regenerate or extend them the same way if the
schema evolves.
