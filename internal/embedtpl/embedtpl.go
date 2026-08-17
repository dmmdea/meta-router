// Package embedtpl is the per-model embedding template registry (W9-P item 1).
//
// Embedding models are prompt-trained: each expects its OWN wrapper around
// queries and documents. EmbeddingGemma's card specifies task-prefixed
// prompts on both sides (worth 1–5%+ retrieval per its own documentation);
// Qwen3-Embedding wants an instruct-prefixed query, plain documents,
// last-token pooling and an explicit EOS. Embedding raw text under-serves
// every one of them — and a query templated one way against vectors templated
// another is a silent vector-space mix that ranks garbage while looking
// healthy. So:
//
//   - every known model carries a versioned (query, doc) template pair;
//   - the template version is part of the index identity ("<model>/tplN"),
//     recorded in the index's Model field at build time;
//   - query-time lookup is by the exact (model, version) the index records,
//     so an index built under an older template version stays servable by a
//     newer binary for as long as the registry keeps that version;
//   - an identity naming a template this binary does not know is an ERROR,
//     never a raw-text guess — the caller refuses retrieval (fail-open) and
//     the operator rebuilds the index or updates the binary.
//
// The W9-P spec names a (queryTemplate, docTemplate, postprocess) triple;
// postprocess is deliberately not represented until a model needs one —
// cosine ranking is scale-invariant, so L2 normalization is moot, and no
// other postprocess exists day one.
package embedtpl

import (
	"fmt"
	"regexp"
	"strings"
)

// TplV1 is the first template version. Template bytes are IMMUTABLE per
// version: any change to a template's output must mint a new version, never
// mutate an existing one — deployed indexes name their version and stay
// servable only while the binary still produces those exact bytes.
const TplV1 = "tpl1"

const (
	// qwenEOS is Qwen3-Embedding's EOS token; its card requires it appended
	// to every input (query AND document) under last-token pooling. TWO
	// things must be verified once against the served endpoint (llama-server
	// --pooling last) before any measurement — see the W9-P program spec §1:
	// (a) that the server does not ALSO append one (double EOS), and (b)
	// that this literal string TOKENIZES to the special EOS token rather
	// than ordinary text tokens (query /tokenize or compare token counts) —
	// under last-token pooling the pooled vector is whatever token actually
	// lands last, so both failures silently degrade every embedding. Either
	// finding mints tpl2.
	qwenEOS = "<|endoftext|>"
	// qwenTask is the job-specific instruction baked into the qwen3 tpl1
	// query template (the card's f"Instruct: {task}\nQuery:{q}" shape).
	// Part of the template bytes: changing it is a new template version.
	qwenTask = "Given a user prompt, retrieve skill descriptions relevant to accomplishing it"
)

// Spec is one resolved embedding configuration: which model to request from
// /v1/embeddings and how to wrap queries and documents before embedding.
// Version "" means untemplated — raw text on both sides, the identity every
// index built before this package carries.
type Spec struct {
	Model   string
	Version string
	Query   func(string) string
	Doc     func(string) string
}

// Raw is the untemplated spec for a model: queries and documents embed
// byte-for-byte as given, and Identity() is the bare model name.
func Raw(model string) Spec {
	return Spec{Model: model}
}

// Identity is the string recorded in the index's Model field: the bare model
// for an untemplated spec, "<model>/<version>" for a templated one.
func (s Spec) Identity() string {
	if s.Version == "" {
		return s.Model
	}
	return s.Model + "/" + s.Version
}

// ApplyQuery templates a query. Nil-safe: a zero-value or Raw spec passes the
// text through untouched. The hook calls this inside the worker goroutine
// that carries its exit-0 contract, where a nil-func panic could never be
// recovered — so the nil check lives here, not at every call site.
func (s Spec) ApplyQuery(q string) string {
	if s.Query == nil {
		return q
	}
	return s.Query(q)
}

// ApplyDoc templates a document (same nil-safety as ApplyQuery).
func (s Spec) ApplyDoc(d string) string {
	if s.Doc == nil {
		return d
	}
	return s.Doc(d)
}

// lookup resolves (model, version) against the registry. version "" means
// "the current template for this model" (build time); a non-empty version
// must match exactly (query time against a recorded index identity).
func lookup(model, version string) (Spec, bool) {
	switch {
	case model == "embeddinggemma":
		if version == "" || version == TplV1 {
			// EmbeddingGemma model card, retrieval task prompts:
			// query "task: search result | query: {q}", document
			// "title: none | text: {d}" (no title metadata exists here).
			return Spec{
				Model:   model,
				Version: TplV1,
				Query:   func(q string) string { return "task: search result | query: " + q },
				Doc:     func(d string) string { return "title: none | text: " + d },
			}, true
		}
	case strings.HasPrefix(model, "qwen3-embedding"):
		// One family entry spans size/quant variants (qwen3-embedding-0.6b,
		// qwen3-embedding-4b-q4, …): the card's format is identical across
		// them. Instruct-prefixed query (no space after "Query:" — that is
		// the card's own f-string), documents plain; EOS on BOTH sides for
		// last-token pooling.
		if version == "" || version == TplV1 {
			return Spec{
				Model:   model,
				Version: TplV1,
				Query:   func(q string) string { return "Instruct: " + qwenTask + "\nQuery:" + q + qwenEOS },
				Doc:     func(d string) string { return d + qwenEOS },
			}, true
		}
	}
	return Spec{}, false
}

// Current returns the newest template for a model — what `mr-index build`
// records into a fresh templated index. false when the registry has no entry
// for the model.
func Current(model string) (Spec, bool) { return lookup(model, "") }

// Lookup returns the template for an exact (model, version) pair — what the
// hook needs to embed queries against an index recording that identity.
func Lookup(model, version string) (Spec, bool) {
	if version == "" {
		return Spec{}, false
	}
	return lookup(model, version)
}

// tplVerRe pins what counts as a template-version suffix. Only a trailing
// "/tplN" segment is an identity split — a model id that itself contains a
// slash (HF-style "org/model") parses as a bare model, not as a version.
var tplVerRe = regexp.MustCompile(`^tpl\d+$`)

// ParseIdentity splits an index identity into model and template version.
// "embeddinggemma" → ("embeddinggemma", ""); "qwen3-embedding-4b-q4/tpl1" →
// ("qwen3-embedding-4b-q4", "tpl1").
func ParseIdentity(id string) (model, version string) {
	if i := strings.LastIndex(id, "/"); i >= 0 && tplVerRe.MatchString(id[i+1:]) {
		return id[:i], id[i+1:]
	}
	return id, ""
}

// SpecForIndex resolves an index's recorded Model identity to the spec that
// must embed queries against its vectors:
//
//   - "" (an index predating the Model field): every such index was built
//     untemplated by the embeddinggemma-only code path → Raw("embeddinggemma").
//   - "<model>" (no version): untemplated → Raw(model).
//   - "<model>/tplN": the registry must know that exact pair. An unknown
//     version is an error, never a raw-text fallback — the caller refuses
//     retrieval (fail-open, quota hint only) rather than mix vector spaces.
func SpecForIndex(identity string) (Spec, error) {
	model, version := ParseIdentity(identity)
	if version == "" {
		if model == "" {
			model = "embeddinggemma"
		}
		return Raw(model), nil
	}
	if s, ok := Lookup(model, version); ok {
		return s, nil
	}
	return Spec{}, fmt.Errorf("index built with embedding template %s/%s, unknown to this binary — rebuild the index (mr-index build) or update the binary", model, version)
}

// RerankSpec formats (query, document) pairs for a specific cross-encoder.
// Same registry idea as Spec: bge-reranker-v2-m3 takes both sides raw,
// Qwen3-Reranker is instruction-formatted.
//
// Unlike Spec.Version, RerankSpec.Version is INFORMATIONAL (a label for eval
// output rows): rerank scores are computed per query and never persisted, so
// there is no rerank index identity, no guard, and no mismatch branch —
// nothing enforces this field.
type RerankSpec struct {
	Model   string
	Version string
	Query   func(string) string
	Doc     func(string) string
}

// RerankRaw is the unformatted rerank spec — bge-reranker-v2-m3's correct
// treatment, and today's only production reranker.
func RerankRaw(model string) RerankSpec {
	return RerankSpec{Model: model}
}

// ApplyQuery formats a rerank query (nil-safe, as Spec.ApplyQuery).
func (r RerankSpec) ApplyQuery(q string) string {
	if r.Query == nil {
		return q
	}
	return r.Query(q)
}

// ApplyDoc formats a rerank document (nil-safe).
func (r RerankSpec) ApplyDoc(d string) string {
	if r.Doc == nil {
		return d
	}
	return r.Doc(d)
}

// RerankFor returns the formatting spec for a reranker model: a registry
// entry when one exists, else raw pass-through.
func RerankFor(model string) RerankSpec {
	if strings.HasPrefix(model, "qwen3-reranker") {
		// Qwen3-Reranker's card formats the user turn as
		// "<Instruct>: {task}\n<Query>: {q}\n<Document>: {d}"; the chat
		// scaffolding (system prompt, im_start/end, think block) belongs to
		// the serving layer (llama-server --pooling rank, official ggml-org
		// GGUF only — community conversions are broken, see the W9-P spec
		// §3). UNVERIFIED against a served endpoint until the bake-off:
		// verify the split of labor once before measuring anything with it.
		return RerankSpec{
			Model:   model,
			Version: TplV1,
			Query:   func(q string) string { return "<Instruct>: " + qwenTask + "\n<Query>: " + q },
			Doc:     func(d string) string { return "<Document>: " + d },
		}
	}
	return RerankRaw(model)
}
