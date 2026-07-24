package render

import (
	"encoding/json"
	"sort"
	"strings"
)

// RawBlockState is one blockstates/<block>.json file. A block uses exactly one
// of the two forms: "variants" (pick one model per state) or "multipart"
// (overlay every matching part). Both map a block's property values
// (facing=north, half=bottom, …) to the model(s) that render it.
type RawBlockState struct {
	Variants  map[string]json.RawMessage `json:"variants"`
	Multipart []RawMultipart             `json:"multipart"`
}

// RawMultipart is one entry of a multipart blockstate: apply these model(s)
// when the (optional) condition matches the block's properties.
type RawMultipart struct {
	When  json.RawMessage `json:"when"`  // absent = always; object of prop→"a|b", or {"OR":[…]} / {"AND":[…]}
	Apply json.RawMessage `json:"apply"` // ModelRef or []ModelRef
}

// ModelRef names a model to render plus its blockstate-level transform.
type ModelRef struct {
	Model  string `json:"model"` // resource location, e.g. "minecraft:block/stone"
	X      int    `json:"x"`     // rotation about X, degrees (0/90/180/270)
	Y      int    `json:"y"`     // rotation about Y
	UVLock bool   `json:"uvlock"`
	Weight int    `json:"weight"`
}

// Chosen returns the model refs that render a block in the given property
// state. For a "variants" block that is a single ref (the first of any
// weighted list). For a "multipart" block it is one ref per matching part.
// Model locations are normalized; an empty result means no rule matched.
func (bs *RawBlockState) Chosen(props map[string]string) []ModelRef {
	if len(bs.Variants) > 0 {
		key := bestVariantKey(bs.Variants, props)
		if key == nil {
			return nil
		}
		refs := parseRefs(bs.Variants[*key])
		if len(refs) == 0 {
			return nil
		}
		return refs[:1] // weighted pick → take the first deterministically
	}

	var out []ModelRef
	for _, mp := range bs.Multipart {
		if !whenMatches(mp.When, props) {
			continue
		}
		if refs := parseRefs(mp.Apply); len(refs) > 0 {
			out = append(out, refs[0])
		}
	}
	return out
}

// AllRefs returns every model referenced anywhere in the blockstate (across all
// variants and multipart parts), deduplicated by model+rotation. Unlike Chosen
// it ignores the property state — it is for validation and atlas pre-scan
// (which textures/models the block can ever use), not for rendering one state.
func (bs *RawBlockState) AllRefs() []ModelRef {
	seen := map[ModelRef]bool{}
	var out []ModelRef
	add := func(refs []ModelRef) {
		for _, r := range refs {
			key := ModelRef{Model: r.Model, X: r.X, Y: r.Y}
			if !seen[key] {
				seen[key] = true
				out = append(out, r)
			}
		}
	}
	for _, raw := range bs.Variants {
		add(parseRefs(raw))
	}
	for _, mp := range bs.Multipart {
		add(parseRefs(mp.Apply))
	}
	return out
}

// bestVariantKey finds the variant key whose constraints are all satisfied by
// props. The empty key "" is the catch-all. When several match, the most
// specific (most constraints) wins, which is how vanilla disambiguates. It
// returns nil if nothing matches.
func bestVariantKey(variants map[string]json.RawMessage, props map[string]string) *string {
	bestScore := -1
	var best *string
	for key := range variants {
		ok, score := variantKeyMatches(key, props)
		if ok && score > bestScore {
			bestScore = score
			k := key
			best = &k
		}
	}
	return best
}

// variantKeyMatches reports whether a variant key ("facing=north,half=bottom",
// or "" for catch-all) is satisfied by props, and how many constraints it
// carries (its specificity).
func variantKeyMatches(key string, props map[string]string) (bool, int) {
	if key == "" {
		return true, 0
	}
	n := 0
	for _, pair := range strings.Split(key, ",") {
		eq := strings.IndexByte(pair, '=')
		if eq < 0 {
			return false, 0
		}
		name, want := pair[:eq], pair[eq+1:]
		if props[name] != want {
			return false, 0
		}
		n++
	}
	return true, n
}

// whenMatches evaluates a multipart "when" clause against props. Supported:
// absent (always true), {"OR":[clause…]}, {"AND":[clause…]}, and a plain
// property map whose values may be "|"-separated alternatives.
func whenMatches(raw json.RawMessage, props map[string]string) bool {
	if len(raw) == 0 {
		return true
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return false
	}
	if or, ok := obj["OR"]; ok {
		var clauses []json.RawMessage
		_ = json.Unmarshal(or, &clauses)
		for _, c := range clauses {
			if whenMatches(c, props) {
				return true
			}
		}
		return false
	}
	if and, ok := obj["AND"]; ok {
		var clauses []json.RawMessage
		_ = json.Unmarshal(and, &clauses)
		for _, c := range clauses {
			if !whenMatches(c, props) {
				return false
			}
		}
		return true
	}
	// plain property constraints: every listed prop must match one alternative.
	for name, rawVal := range obj {
		var val string
		if err := json.Unmarshal(rawVal, &val); err != nil {
			// values are occasionally booleans/ints in JSON; fall back to raw.
			val = strings.Trim(string(rawVal), `"`)
		}
		got, ok := props[name]
		if !ok {
			return false
		}
		matched := false
		for _, alt := range strings.Split(val, "|") {
			if got == alt {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

// parseRefs decodes a variant/apply value that is either a single ModelRef
// object or an array of them, and normalizes model locations.
func parseRefs(raw json.RawMessage) []ModelRef {
	if len(raw) == 0 {
		return nil
	}
	trimmed := strings.TrimLeft(string(raw), " \t\r\n")
	var refs []ModelRef
	if strings.HasPrefix(trimmed, "[") {
		if err := json.Unmarshal(raw, &refs); err != nil {
			return nil
		}
	} else {
		var one ModelRef
		if err := json.Unmarshal(raw, &one); err != nil {
			return nil
		}
		refs = []ModelRef{one}
	}
	for i := range refs {
		refs[i].Model = normalizeLoc(refs[i].Model)
	}
	return refs
}

// PropsKey renders a property map into the canonical "a=b,c=d" form with keys
// sorted — handy for caching a resolved model per distinct state.
func PropsKey(props map[string]string) string {
	if len(props) == 0 {
		return ""
	}
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(props[k])
	}
	return b.String()
}
