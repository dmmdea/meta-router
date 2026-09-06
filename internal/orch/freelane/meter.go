package freelane

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// groqDurationRe accepts Groq's reset format ("2m59.56s", "7.66s", "1h2m",
// and a leading day count the docs do not show but a 1K/day window could
// plausibly render as "1d").
var groqDurationRe = regexp.MustCompile(`^(?:(\d+)d)?(?:(\d+)h)?(?:(\d+)m)?(?:(\d+(?:\.\d+)?)s)?$`)

// ParseGroqDuration parses the x-ratelimit-reset-* value. ok=false for empty
// or unrecognised input — the caller then falls back to the calendar reset
// rather than inventing one.
func ParseGroqDuration(s string) (time.Duration, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	m := groqDurationRe.FindStringSubmatch(s)
	if m == nil || (m[1] == "" && m[2] == "" && m[3] == "" && m[4] == "") {
		return 0, false
	}
	var d time.Duration
	if m[1] != "" {
		n, _ := strconv.Atoi(m[1])
		d += time.Duration(n) * 24 * time.Hour
	}
	if m[2] != "" {
		n, _ := strconv.Atoi(m[2])
		d += time.Duration(n) * time.Hour
	}
	if m[3] != "" {
		n, _ := strconv.Atoi(m[3])
		d += time.Duration(n) * time.Minute
	}
	if m[4] != "" {
		f, _ := strconv.ParseFloat(m[4], 64)
		d += time.Duration(f * float64(time.Second))
	}
	return d, true
}

// NeuronRate is Cloudflare's price of a model in neurons per MILLION tokens,
// input and output (pricing page, fetched 2026-09-06). The free allocation is
// 10,000 neurons/day, so the meter converts the vendor's token usage into
// neurons with this table; an unknown model meters the floor (see the caller)
// and is flagged, never silently zero.
type NeuronRate struct {
	In, Out int64
}

// Neurons converts usage to neurons, rounding UP (a fractional neuron still
// counts against the day; rounding down would under-meter every small call).
func Neurons(u Usage, r NeuronRate) int64 {
	num := u.Prompt*r.In + u.Completion*r.Out
	if num <= 0 {
		return 0
	}
	return (num + 999_999) / 1_000_000
}

// DefaultNeuronRates is the pricing page as of 2026-09-06 for the models the
// vetting named. Overridable per model through orchcfg
// free_cloudflare_neuron_rates; the policy watch hashes the page.
func DefaultNeuronRates() map[string]NeuronRate {
	return map[string]NeuronRate{
		"@cf/openai/gpt-oss-120b":                  {In: 31818, Out: 68182},
		"@cf/openai/gpt-oss-20b":                   {In: 18182, Out: 27273},
		"@cf/meta/llama-3.3-70b-instruct-fp8-fast": {In: 26668, Out: 204805},
		"@cf/meta/llama-4-scout-17b-16e-instruct":  {In: 24545, Out: 77273},
		"@cf/qwen/qwq-32b":                         {In: 60000, Out: 90909},
		"@cf/qwen/qwen3-30b-a3b-fp8":               {In: 4625, Out: 30475},
		"@cf/google/gemma-4-26b-a4b-it":            {In: 9091, Out: 27273},
	}
}
