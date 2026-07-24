// jarcheck provisions the vanilla client jar and validates tachyne-map's asset
// loader against the REAL assets: every blockstate parses, every referenced
// model resolves through its parent chain, and every face texture resolves to
// a PNG that actually exists in the jar. It is a dev/CI smoke test for the
// render package's model layer — not part of the map pod.
//
//	go run ./cmd/jarcheck -version 1.21.11 -v
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"

	"github.com/tachyne/tachyne-map/render"
)

func main() {
	version := flag.String("version", "1.21.11", "Minecraft version to validate against")
	cache := flag.String("cache", defaultCache(), "asset cache dir")
	verbose := flag.Bool("v", false, "list sample failures")
	flag.Parse()

	jar, err := render.EnsureClientJar(*cache, *version, true)
	if err != nil {
		log.Fatalf("provision client jar: %v", err)
	}
	fmt.Printf("client jar: %s\n", jar)

	a, err := render.OpenJar(jar)
	if err != nil {
		log.Fatalf("open jar: %v", err)
	}
	defer a.Close()

	names := a.BlockStateNames()
	sort.Strings(names)
	fmt.Printf("blockstates in jar: %d\n", len(names))

	var (
		parseFail    []string
		missingModel []string
		unresolved   []string // face texture var that didn't resolve
		missingTex   []string // resolved texture with no PNG in jar
		refs, faces  int
		okStates     int
	)

	for _, name := range names {
		bs, err := a.BlockState(name)
		if err != nil {
			parseFail = append(parseFail, name+": "+err.Error())
			continue
		}
		okStates++
		for _, ref := range bs.AllRefs() {
			refs++
			if _, err := a.RawModel(ref.Model); err != nil {
				missingModel = append(missingModel, name+" -> "+ref.Model)
				continue
			}
			m, err := render.Resolve(a, ref.Model)
			if err != nil {
				missingModel = append(missingModel, name+" -> "+ref.Model+" (resolve: "+err.Error()+")")
				continue
			}
			for _, el := range m.Elements {
				for dir, face := range el.Faces {
					if face.Texture == "" {
						continue
					}
					faces++
					loc := m.ResolveTexture(face.Texture)
					if loc == "" {
						unresolved = append(unresolved, fmt.Sprintf("%s %s.%s %q", name, ref.Model, dir, face.Texture))
						continue
					}
					if !a.HasTexture(loc) {
						missingTex = append(missingTex, fmt.Sprintf("%s %s.%s -> %s", name, ref.Model, dir, loc))
					}
				}
			}
		}
	}

	fmt.Printf("\n== results ==\n")
	fmt.Printf("blockstates parsed ok : %d / %d\n", okStates, len(names))
	fmt.Printf("model refs checked    : %d\n", refs)
	fmt.Printf("faces checked         : %d\n", faces)
	fmt.Printf("parse failures        : %d\n", len(parseFail))
	fmt.Printf("missing/failed models : %d\n", len(missingModel))
	fmt.Printf("unresolved textures   : %d\n", len(unresolved))
	fmt.Printf("missing texture PNGs  : %d\n", len(missingTex))

	sample := func(title string, xs []string) {
		if len(xs) == 0 {
			return
		}
		n := len(xs)
		if n > 20 {
			n = 20
		}
		fmt.Printf("\n-- %s (showing %d) --\n", title, n)
		for _, x := range xs[:n] {
			fmt.Printf("  %s\n", x)
		}
	}
	if *verbose {
		sample("parse failures", parseFail)
		sample("missing models", missingModel)
		sample("unresolved textures", unresolved)
		sample("missing texture PNGs", missingTex)
	}

	if len(parseFail)+len(missingModel)+len(unresolved)+len(missingTex) == 0 {
		fmt.Printf("\nALL CLEAN ✅\n")
	}
}

func defaultCache() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cache", "tachyne-map")
}
