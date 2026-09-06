package orchcfg

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dmmdea/meta-router/internal/orch/freelane"
)

// Free-lane config (W4): defaults, hand-edit backfill, overlay lookup, the
// gemini allowlist semantics and the neuron-rate fallback order.
func TestFreeLaneConfig(t *testing.T) {
	d := Defaults()
	if d.FreeLanesOff || d.FreeMaxTokens != 4096 || d.FreeTimeoutSec != 180 || len(d.FreeGeminiAllowClasses) != 0 {
		t.Fatalf("defaults: %+v", d)
	}
	if d.FreeGeminiClassAllowed("mechanical-text") {
		t.Fatal("an EMPTY allowlist must allow nothing (gemini stays gated until the operator lists classes)")
	}
	if l := d.FreeLimits("groq"); l != (freelane.Limits{}) {
		t.Fatalf("no overlay → zero Limits: %+v", l)
	}
	p := filepath.Join(t.TempDir(), "config.json")
	cfg := `{"free_max_tokens":0,"free_timeout_sec":0,
	  "free_providers":{"groq":{"rpm":10,"daily_cap":500},"nim":{"off":true}},
	  "free_cloudflare_account_id":"acct-1",
	  "free_cloudflare_neuron_rates":{"@cf/openai/gpt-oss-120b":{"in":1,"out":2},"@cf/x/new-model":{"in":3,"out":4}},
	  "free_gemini_allow_classes":["mechanical-text","doc-summarize"]}`
	if err := os.WriteFile(p, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	c := Load(p)
	if c.FreeMaxTokens != 4096 || c.FreeTimeoutSec != 180 {
		t.Fatalf("explicit zeros are hand-edit damage — backfill: %+v", c)
	}
	if l := c.FreeLimits("groq"); l.RPM != 10 || l.DailyCap != 500 || l.Off {
		t.Fatalf("groq overlay: %+v", l)
	}
	if l := c.FreeLimits("nim"); !l.Off || l.RPM != 0 {
		t.Fatalf("nim overlay: %+v", l)
	}
	if c.FreeCloudflareAccountID != "acct-1" {
		t.Fatalf("account id: %q", c.FreeCloudflareAccountID)
	}
	if r, ok := c.FreeNeuronRate("@cf/openai/gpt-oss-120b"); !ok || r != (freelane.NeuronRate{In: 1, Out: 2}) {
		t.Fatalf("operator table must win over the registry: %+v %v", r, ok)
	}
	if r, ok := c.FreeNeuronRate("@cf/x/new-model"); !ok || r.Out != 4 {
		t.Fatalf("operator table may add models: %+v %v", r, ok)
	}
	if r, ok := c.FreeNeuronRate("@cf/openai/gpt-oss-20b"); !ok || r != freelane.DefaultNeuronRates()["@cf/openai/gpt-oss-20b"] {
		t.Fatalf("registry fallback: %+v %v", r, ok)
	}
	if _, ok := c.FreeNeuronRate("@cf/nobody/knows"); ok {
		t.Fatal("an unknown model must report ok=false so the caller meters a floor and warns")
	}
	if !c.FreeGeminiClassAllowed("mechanical-text") || c.FreeGeminiClassAllowed("hard-repo") || c.FreeGeminiClassAllowed("") {
		t.Fatalf("gemini allowlist semantics: %+v", c.FreeGeminiAllowClasses)
	}
}
