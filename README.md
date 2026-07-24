# tachyne-map

A native-Go **3D web map** for [tachyne](https://github.com/tachyne/tachyne-world),
a from-scratch Minecraft-compatible server. tachyne-map renders the engine's
world into browser tiles **directly in Go** — no Java, no intermediate Anvil
save. It reads the engine's world, meshes each chunk with real vanilla block
models and textures, and serves a WebGL viewer as its own Kubernetes pod.

## Status

Early, and built in milestones. **M1 (the asset/model loader) is complete** and
validated against the entire vanilla block set (1168 blockstates, ~60k faces,
zero unresolved).

- [x] **M1** — client-jar provisioning + blockstate/block-model parsing
      (parent flattening, texture-variable resolution)
- [ ] **M2** — texture atlas + biome colormaps + per-chunk mesher + tile format
- [ ] **M3** — tile-server pod + three.js viewer
- [ ] **M4** — incremental re-render from the engine's block-change stream

## How it fits

tachyne's engine (`tachyne-world`) is a pure function of `(seed, edits)`, so the
map pod mounts the world data **read-only**, rebuilds chunks, and renders them.
It reads block state, biomes, per-block light, and heightmaps through a small
public `worldread` facade in tachyne-world, and picks up live edits from the
engine's message bus — so the map updates in near-real-time.

## Assets

Block models and textures come from Mojang's **client jar**, fetched at runtime
and cached locally — **never redistributed in this repo**. Provisioning is gated
on an operator flag asserting acceptance of Mojang's EULA.

## Develop

```sh
go test ./...              # unit tests (no network)
go run ./cmd/jarcheck -v   # validate the loader against the real client jar
```

## Credits & license

Written by Wesley Channon. Licensed under Apache-2.0 — see [LICENSE](LICENSE).

tachyne is an **unofficial fan project** — not affiliated with, endorsed by, or
associated with Mojang, Microsoft, or the developers of Minecraft. "Minecraft"
is a trademark of Mojang Synergies AB.
