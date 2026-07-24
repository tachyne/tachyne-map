package render

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// Mojang's version manifest; each version's own JSON carries the client-jar
// download URL. We read the client jar because block models and textures live
// only there — the server jars on disk (~/vanilla/server-*.jar) have no assets.
const versionManifestURL = "https://piston-meta.mojang.com/mc/game/version_manifest_v2.json"

// EnsureClientJar returns the path to the vanilla client jar for version,
// downloading it into cacheDir on first use. Because the jar is Mojang's
// proprietary asset (not ours to redistribute), callers must pass accept=true
// to assert the operator accepts Mojang's EULA — mirroring daemons/bluemap's
// -accept-download gate.
func EnsureClientJar(cacheDir, version string, accept bool) (string, error) {
	jar := filepath.Join(cacheDir, "client-"+version+".jar")
	if _, err := os.Stat(jar); err == nil {
		return jar, nil
	}
	if !accept {
		return "", fmt.Errorf("client jar for %s not cached and EULA not accepted "+
			"(pass -accept-download / MAP_ACCEPT_DOWNLOAD=true to fetch Mojang assets)", version)
	}
	url, err := clientJarURL(version)
	if err != nil {
		return "", err
	}
	if err := download(url, jar); err != nil {
		return "", err
	}
	return jar, nil
}

// clientJarURL resolves version → its client.jar download URL via the manifest.
func clientJarURL(version string) (string, error) {
	var manifest struct {
		Versions []struct {
			ID  string `json:"id"`
			URL string `json:"url"`
		} `json:"versions"`
	}
	if err := getJSON(versionManifestURL, &manifest); err != nil {
		return "", fmt.Errorf("version manifest: %w", err)
	}
	verURL := ""
	for _, v := range manifest.Versions {
		if v.ID == version {
			verURL = v.URL
			break
		}
	}
	if verURL == "" {
		return "", fmt.Errorf("version %q not in Mojang manifest", version)
	}
	var meta struct {
		Downloads struct {
			Client struct {
				URL string `json:"url"`
			} `json:"client"`
		} `json:"downloads"`
	}
	if err := getJSON(verURL, &meta); err != nil {
		return "", fmt.Errorf("version meta %s: %w", version, err)
	}
	if meta.Downloads.Client.URL == "" {
		return "", fmt.Errorf("version %q has no client download", version)
	}
	return meta.Downloads.Client.URL, nil
}

// Assets reads block models, blockstates, and textures from an opened client
// jar. It implements ModelSource. Close it when done.
type Assets struct {
	zr    *zip.ReadCloser
	files map[string]*zip.File // archive path → entry
}

// OpenJar opens a client jar for asset reads.
func OpenJar(path string) (*Assets, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil, err
	}
	a := &Assets{zr: zr, files: make(map[string]*zip.File, len(zr.File))}
	for _, f := range zr.File {
		a.files[f.Name] = f
	}
	return a, nil
}

// Close releases the underlying jar.
func (a *Assets) Close() error { return a.zr.Close() }

// read returns the bytes of an archive entry.
func (a *Assets) read(name string) ([]byte, error) {
	f, ok := a.files[name]
	if !ok {
		return nil, fmt.Errorf("asset %q not found in jar", name)
	}
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// assetPath maps a resource location + asset kind to its archive path:
// ("minecraft:block/stone", "models", "json") → "assets/minecraft/models/block/stone.json".
func assetPath(loc, kind, ext string) string {
	ns, name := "minecraft", loc
	if i := strings.IndexByte(loc, ':'); i >= 0 {
		ns, name = loc[:i], loc[i+1:]
	}
	return fmt.Sprintf("assets/%s/%s/%s.%s", ns, kind, name, ext)
}

// RawModel fetches and decodes a block-model JSON by location. Satisfies
// ModelSource, so Resolve can walk parent chains straight off the jar.
func (a *Assets) RawModel(loc string) (*RawModel, error) {
	b, err := a.read(assetPath(loc, "models", "json"))
	if err != nil {
		return nil, err
	}
	var m RawModel
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("model %s: %w", loc, err)
	}
	return &m, nil
}

// BlockState fetches and decodes a blockstate JSON by block name
// (e.g. "stone" or "minecraft:oak_stairs").
func (a *Assets) BlockState(name string) (*RawBlockState, error) {
	b, err := a.read(assetPath(name, "blockstates", "json"))
	if err != nil {
		return nil, err
	}
	var bs RawBlockState
	if err := json.Unmarshal(b, &bs); err != nil {
		return nil, fmt.Errorf("blockstate %s: %w", name, err)
	}
	return &bs, nil
}

// Texture returns the raw PNG bytes for a texture location
// (e.g. "minecraft:block/stone").
func (a *Assets) Texture(loc string) ([]byte, error) {
	return a.read(assetPath(loc, "textures", "png"))
}

// HasTexture reports whether a texture PNG exists in the jar.
func (a *Assets) HasTexture(loc string) bool {
	_, ok := a.files[assetPath(loc, "textures", "png")]
	return ok
}

// BlockStateNames lists every minecraft: blockstate in the jar (bare names,
// e.g. "stone", "oak_stairs"), suitable for BlockState.
func (a *Assets) BlockStateNames() []string {
	const pre, suf = "assets/minecraft/blockstates/", ".json"
	var names []string
	for name := range a.files {
		if strings.HasPrefix(name, pre) && strings.HasSuffix(name, suf) {
			names = append(names, name[len(pre):len(name)-len(suf)])
		}
	}
	return names
}

// download fetches url to path atomically (temp file + rename).
func download(url, path string) error {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// getJSON fetches url and decodes the JSON body into v.
func getJSON(url string, v any) error {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}
