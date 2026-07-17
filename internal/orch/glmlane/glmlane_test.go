package glmlane

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/meta-router/internal/orch/claudelane"
)

// claudelaneParse is the reuse contract, pinned by name: the GLM result schema
// is claudelane-parser-compatible (fixture-proven, both models) — the lane
// ships NO parser of its own.
var claudelaneParse = claudelane.Parse

func TestTokenReadTrimsAndNeverEchoesValue(t *testing.T) {
	p := filepath.Join(t.TempDir(), "glm-token")
	if err := os.WriteFile(p, []byte("sk-SECRET-VALUE\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tok, err := Token(p)
	if err != nil || tok != "sk-SECRET-VALUE" {
		t.Fatalf("token read: %q %v", tok, err)
	}
	_, err = Token(filepath.Join(t.TempDir(), "missing"))
	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("missing-token error must name the path, never any value: %v", err)
	}
}

// R10: an empty/whitespace-only token file is a config error naming the path —
// dispatching with an empty ANTHROPIC_AUTH_TOKEN would fall back to whatever
// auth the environment carries (silent lane cross-wiring).
func TestTokenEmptyFileIsConfigError(t *testing.T) {
	p := filepath.Join(t.TempDir(), "glm-token")
	if err := os.WriteFile(p, []byte("  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Token(p); err == nil || !strings.Contains(err.Error(), p) {
		t.Fatalf("empty token file must error naming the path: %v", err)
	}
}

func TestEnvPinsEverything(t *testing.T) {
	env := Env("tok123", "glm-5.2")
	joined := strings.Join(env, "\n")
	for _, want := range []string{
		"ANTHROPIC_BASE_URL=https://api.z.ai/api/anthropic",
		"ANTHROPIC_AUTH_TOKEN=tok123",
		"API_TIMEOUT_MS=3000000",
		"ANTHROPIC_DEFAULT_OPUS_MODEL=glm-5.2",
		"ANTHROPIC_DEFAULT_SONNET_MODEL=glm-5.2",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL=glm-4.7",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("env missing %q:\n%s", want, joined)
		}
	}
}

func TestGLMFixturesParseThroughClaudelane(t *testing.T) { // the reuse contract, pinned
	for _, f := range []string{"result-glm-5.2.json", "result-glm-4.7.json"} {
		b, err := os.ReadFile("../../../testdata/fixtures/glm/" + f)
		if err != nil {
			t.Fatalf("fixture %s: %v", f, err)
		}
		o := claudelaneParse(b) // thin alias to claudelane.Parse — asserts schema compatibility
		if o.Class != "ok" || len(o.ModelUsage) != 1 {
			t.Fatalf("%s must parse ok with single-model attribution: %+v", f, o)
		}
	}
}
