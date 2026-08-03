# Contributing

Contributions are welcome — bug reports, fixes, features, and docs alike.

## Ground rules

- **Build/test before you push**: `go build ./... && go vet ./... && go test ./...`
  must be clean, and `gofmt -w` run on touched files. CI enforces all three.
- **The map is a READER.** It opens the world through `tachyne-world`'s
  read-only `worldread` facade and never writes to it. A change that mutates
  world data belongs in the engine, not here.
- **Mesher caches are shared across goroutines.** Tiles are built in parallel;
  any cache you add must be mutex-guarded, and a race in `go test -race` is a
  blocker, not a flake.
- **Client assets are not vendored.** Block models and textures are read from
  the player's own Minecraft jar at run time; do not commit Mojang assets.
- **Vanilla behavior facts only.** Rendering constants must come from
  observable behavior or published data — never copied code.

## Licensing of contributions

The project is Apache-2.0. Per its section 5, any contribution you
intentionally submit is licensed under the same terms, with no separate CLA.
Please make sure you have the right to submit what you contribute.

## Getting oriented

The README explains the tile format, the viewer and every flag;
`cmd/jarcheck` and `cmd/meshcheck` are the offline tools for inspecting asset
loading and meshing without running the server.
