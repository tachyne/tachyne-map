# tachyne-map

A native-Go **3D web map** for [tachyne](https://github.com/tachyne/tachyne-world),
a from-scratch Minecraft-compatible server. tachyne-map renders the world into
browser tiles **directly in Go** — no Java, no intermediate save format. It reads
the engine's world, meshes each chunk with real vanilla block models and
textures, and serves a WebGL viewer as its own Kubernetes pod.

It follows the running server: blocks placed in game appear on the map within a
second or so, and players show as live markers.

## What it does

- **Native Go rendering.** Blockstates, block models (parent chains, texture
  variables, element rotation), a stitched texture atlas, and biome colormaps
  are all parsed and meshed in Go. Face culling, per-block sky/block light,
  ambient shading, and biome tint are baked into the tile geometry, so the
  browser needs no lighting at all.
- **Explorable, not a fixed picture.** The viewer streams tiles around the
  camera and unloads them behind you, so you can pan across the world
  indefinitely with bounded memory. The pod meshes any chunk on demand.
- **Live.** The map subscribes to the engine's block-change events and re-meshes
  only the affected tiles, pushing invalidations to browsers over SSE. It also
  shows player positions, and serves mob positions for markers.
- **Reads the world, never writes it.** The engine stays the only writer: the
  map opens the world read-only through tachyne-world's `worldread` facade,
  which has no save path.

## How it fits

tachyne's world is a pure function of `(seed, edits)`. At boot the map asks the
running engine for its **seed** over the NATS bus (so it can't drift from the
world players are in), loads the engine's **edit overlay** read-only so existing
builds appear, and then follows `mc.event.block_change` to stay current. Nothing
is shared but a read-only file mount and a message bus.

```
engine (tachyne-world) ──bus: seed, block_change, players, mobs──► tachyne-map
world.gob (read-only) ─────────────────────────────────────────►   │
                                                                    ▼
                                         tiles + atlas + viewer (HTTP :8100)
```

## HTTP endpoints

| Path | What |
|---|---|
| `/` | the embedded three.js viewer |
| `/manifest.json` | dimension, spawn, atlas metadata |
| `/atlas.png` | stitched block-texture atlas |
| `/tile/{dim}/{cx}/{cz}.json` | one chunk's baked geometry (meshed on demand, cached, gzipped) |
| `/events` | Server-Sent Events: tile invalidations when the world changes |
| `/players.json` | live player positions |
| `/mobs.json` | live mob positions, tagged hostile/passive |
| `/healthz` | liveness |

## Configuration

Every flag has an environment variable, so a pod can be configured without args.

| Flag | Env | Default | Purpose |
|---|---|---|---|
| `-addr` | `MAP_ADDR` | `:8100` | HTTP listen address |
| `-nats` | `NATS_URL` | *(none)* | engine bus: seed discovery + live updates |
| `-world` | `MAP_WORLD` | *(none)* | engine `world.gob`, read-only, to show player builds |
| `-seed` | `MAP_SEED` | `1` | fallback seed when the bus is unavailable |
| `-radius` | `MAP_RADIUS` | `8` | advertised region radius, in chunks |
| `-version` | `MAP_VERSION` | `1.21.11` | Minecraft asset version |
| `-cache` | `MAP_CACHE` | `/var/cache/tachyne-map` | asset cache directory |
| `-accept-download` | `MAP_ACCEPT_DOWNLOAD` | `false` | assert acceptance of Mojang's EULA |

The bus and the world file are both optional: with neither, the map serves a
static, terrain-only view of a seed.

## Assets

Block models and textures come from Mojang's **client jar**, fetched at runtime
and cached locally — **never redistributed in this repo**. Provisioning is gated
on an operator flag asserting acceptance of Mojang's EULA.

## Develop

```sh
go test ./...              # unit tests (no network)
go run ./cmd/jarcheck -v   # validate the asset loader against the real client jar
go run ./cmd/meshcheck -seed 1 -radius 6 -out web/preview   # mesh a region
go run . -accept-download -seed 1                           # run the map locally
```

`cmd/meshcheck` can also write a top-down preview PNG (`-png`), which is a quick
way to sanity-check terrain, water, and biome tint without a browser.

## Status

The renderer, the streaming viewer, live updates, and player markers are all
working and deployed. Known gaps: mob markers are served but not yet drawn;
block-change events carry no dimension, so live updates are overworld-only; and
distant terrain is not yet level-of-detail simplified, which bounds how far you
can usefully zoom out.

## Acknowledgements

tachyne-map is inspired by [BlueMap](https://bluemap.bluecolored.de) by Blue
(BlueColored) — the excellent 3D Minecraft web map that set the bar for what a
browser-based world view can be, and which tachyne used (via its Java renderer)
before this. tachyne-map is an independent, from-scratch Go implementation
rather than a port, but the concept is BlueMap's. BlueMap is open source under
the MIT license; if any BlueMap-derived code lands here, it will be credited in
a `NOTICE` file per that license.

The viewer bundles [three.js](https://threejs.org) (MIT) — see `web/NOTICE`.

## License

Licensed under Apache-2.0 — see [LICENSE](LICENSE).

tachyne is an **unofficial fan project** — not affiliated with, endorsed by, or
associated with Mojang, Microsoft, or the developers of Minecraft. "Minecraft"
is a trademark of Mojang Synergies AB.
