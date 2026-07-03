package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/paulmanoni/nexus/extension/config/internal/canonical"
)

// resolveSnapshot is the core merge: given the full source
// content (every app's YAML body + the optional _common body)
// and the (app, profile) request, produce the flattened value
// tree the client will see.
//
// Merge order (later wins):
//
//  1. _common.profiles.default       (cross-app base)
//  2. _common.profiles.<profile>     (cross-app per-env overlay)
//  3. <app>.profiles.default         (app base)
//  4. <app>.profiles.<profile>       (app per-env overlay)
//
// Each step is a deep merge — maps union (recursing into nested
// maps), scalars and arrays replace. Spring Cloud Config's
// behavior, distilled to two tiers instead of four.
func resolveSnapshot(content map[string]appBody, app, profile string) (map[string]any, error) {
	if _, ok := content[app]; !ok {
		return nil, fmt.Errorf("config: app %q not found in source", app)
	}
	out := map[string]any{}

	// _common is optional; absence is fine, an empty value tree
	// just contributes nothing to the merge.
	if common, ok := content["_common"]; ok {
		applyProfileMerge(out, common, profile)
	}
	applyProfileMerge(out, content[app], profile)
	return out, nil
}

// applyProfileMerge applies an appBody's `default` then
// `<profile>` layers onto target in-place. Missing layers are
// no-ops (an app with only a `default` profile still works for
// any requested profile).
func applyProfileMerge(target map[string]any, body appBody, profile string) {
	if base, ok := body.Profiles["default"]; ok {
		deepMerge(target, base)
	}
	if profile != "default" {
		if env, ok := body.Profiles[profile]; ok {
			deepMerge(target, env)
		}
	}
}

// deepMerge applies src onto dst recursively. Rules:
//
//   - Both sides are map[string]any → recurse into each shared key
//   - Either side is non-map → src replaces dst's value
//   - Keys in src absent from dst → added
//   - Keys in dst absent from src → preserved (the whole point of layering)
//
// Arrays are NOT element-wise merged — a `tags: [a, b]` in src
// replaces a `tags: [c, d]` in dst wholesale. Element-wise list
// merging would force operators to fight Go's any-typed semantics
// for a feature most production fleets configure via concatenation
// at the app layer if they need it.
func deepMerge(dst, src map[string]any) {
	for k, v := range src {
		if vm, ok := v.(map[string]any); ok {
			if existing, ok := dst[k].(map[string]any); ok {
				deepMerge(existing, vm)
				continue
			}
		}
		dst[k] = v
	}
}

// snapshotVersion produces a content-addressed identifier for a
// merged value tree. Stable across two servers that produced the
// same tree from the same inputs — relies on canonical-JSON to
// give us byte-identical hash inputs.
//
// Used as Snapshot.Version, returned in the version endpoint, and
// compared by polling clients to short-circuit a full snapshot
// fetch when nothing changed. SHA-256 → 32 bytes → 64-char hex;
// truncated to 16 chars for readability in logs/headers.
func snapshotVersion(values map[string]any) (string, error) {
	// Re-decode through encoding/json so the canonical emitter sees
	// the same generic map shape sign.go uses for signing inputs.
	// Keeps the version stable across the same code paths that
	// produce signatures.
	raw, err := json.Marshal(values)
	if err != nil {
		return "", err
	}
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return "", err
	}
	body, err := canonicalBytes(generic)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:8]), nil
}

// canonicalBytes is the local shim around the internal canonical
// package — pulled out so future changes to the canonical layer
// can keep snapshot.go + merge.go in sync via one edit point.
func canonicalBytes(v map[string]any) ([]byte, error) {
	return canonical.Marshal(v)
}
