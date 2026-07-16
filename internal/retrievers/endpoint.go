package retrievers

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// EndpointEnv pins this machine's embedder without editing the shared
// settings.json that every host syncs from the config repo.
const EndpointEnv = "MR_EMBED_ENDPOINT"

// endpointsFile is the machine-local pin, read from ~/.meta-router/endpoints.json
// (a JSON array of URLs). It sits beside index.json — state that is per-host and
// never committed — which is the point: host-specific wiring stays OUT of the
// shared config repo.
const endpointsFile = "endpoints.json"

// Endpoint is one candidate embedder.
//
// Unverified marks a candidate the operator never actually configured — it came
// from the built-in chain, so we have no promise that an embedder (rather than
// some unrelated service that happens to hold the port) is listening there.
// Those get a cheap /v1/models probe BEFORE any prompt text is POSTed to them;
// see probeIsEmbedder. Explicitly configured candidates (flag/env/file) are the
// operator's word and are used directly.
type Endpoint struct {
	URL        string
	Unverified bool
}

// DefaultEndpoints are tried in order when nothing pins one:
//
//	:11436 — llama-swap, the ecosystem-canonical port (EmbeddingGemma resident).
//	:18793 — a dedicated embedder sidecar, on hosts that run one.
//
// Trying both is what makes a single binary + a single shared settings.json work
// on every machine with zero per-host configuration. A host that serves only one
// of them needs no config at all; the other candidate just refuses the connection
// (microseconds on loopback) and the next one answers.
//
// Because nobody configured these, they are Unverified: we confirm an embedder is
// really there before sending it a prompt.
func DefaultEndpoints() []string {
	return []string{"http://127.0.0.1:11436", "http://127.0.0.1:18793"}
}

// ResolveEndpoints turns an endpoint spec into the ordered candidate URLs.
// Precedence, highest first:
//
//  1. spec (the -endpoint flag) — AUTHORITATIVE: exactly these, never padded
//     with fallbacks, so pinning an endpoint yields that endpoint or an error.
//  2. $MR_EMBED_ENDPOINT — the per-machine env override.
//  3. ~/.meta-router/endpoints.json — the per-machine file pin.
//  4. DefaultEndpoints() — the built-in failover chain.
//
// Layers 1–3 may each be a comma-separated list.
func ResolveEndpoints(spec string) []string {
	eps := make([]string, 0, 2)
	for _, e := range resolveEndpoints(spec) {
		eps = append(eps, e.URL)
	}
	return eps
}

// resolveEndpoints is ResolveEndpoints with the provenance kept, so embed knows
// which candidates were configured by a human and which it guessed.
func resolveEndpoints(spec string) []Endpoint {
	if eps := splitEndpoints(spec); len(eps) > 0 {
		return configured(eps)
	}
	if eps := splitEndpoints(os.Getenv(EndpointEnv)); len(eps) > 0 {
		return configured(eps)
	}
	if eps := fileEndpoints(); len(eps) > 0 {
		return configured(eps)
	}
	out := make([]Endpoint, 0, 2)
	for _, u := range DefaultEndpoints() {
		out = append(out, Endpoint{URL: u, Unverified: true})
	}
	return out
}

func configured(urls []string) []Endpoint {
	out := make([]Endpoint, 0, len(urls))
	for _, u := range urls {
		out = append(out, Endpoint{URL: u})
	}
	return out
}

// fileEndpoints reads the machine-local pin. Any problem (absent, unreadable,
// malformed) yields nothing so the caller falls through to the defaults — a bad
// file must never leave the surfacer with no endpoint at all. Entries are
// normalized individually (never joined into one string) so a URL containing a
// comma cannot be shredded into two bogus candidates.
func fileEndpoints() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	raw, err := os.ReadFile(filepath.Join(home, ".meta-router", endpointsFile))
	if err != nil {
		return nil
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil
	}
	out := make([]string, 0, len(list))
	seen := map[string]bool{}
	for _, p := range list {
		p = strings.TrimRight(strings.TrimSpace(p), "/")
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// splitEndpoints normalizes a comma-separated spec: trims blanks, drops a
// trailing slash (so "http://h:1/" and "http://h:1" are one endpoint, not two
// — the request path is appended by the caller), and dedupes while preserving
// order, so a redundant candidate is never dialed twice.
func splitEndpoints(spec string) []string {
	var out []string
	seen := map[string]bool{}
	for _, p := range strings.Split(spec, ",") {
		p = strings.TrimRight(strings.TrimSpace(p), "/")
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}
