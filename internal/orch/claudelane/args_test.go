package claudelane

import (
	"slices"
	"strings"
	"testing"
)

func TestBuildArgsHardRules(t *testing.T) {
	tests := []struct {
		name    string
		req     RunReq
		wantErr string
		want    []string // must appear
		reject  []string // must never appear
	}{
		{name: "model required", req: RunReq{Prompt: "x"}, wantErr: "model is required"},
		{name: "pins model and json format",
			req:  RunReq{Prompt: "x", Model: "claude-opus-4-8"},
			want: []string{"-p", "x", "--model", "claude-opus-4-8", "--output-format", "json"}},
		{name: "bare forbidden (R10: bare is API-key-only)",
			req: RunReq{Prompt: "x", Model: "m", Extra: []string{"--bare"}}, wantErr: "--bare"},
		{name: "bg forbidden with -p",
			req: RunReq{Prompt: "x", Model: "m", Extra: []string{"--bg"}}, wantErr: "--bg"},
		{name: "effort passes through",
			req:  RunReq{Prompt: "x", Model: "m", Effort: "high"},
			want: []string{"--effort", "high"}},
		{name: "extra passthrough allowed (R11 operator override)",
			req:  RunReq{Prompt: "x", Model: "m", Extra: []string{"--verbose"}},
			want: []string{"--verbose"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			args, err := BuildArgs(tc.req)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			joined := strings.Join(args, " ")
			for _, w := range tc.want {
				if !strings.Contains(joined, w) {
					t.Fatalf("args %v missing %q", args, w)
				}
			}
			for _, r := range tc.reject {
				if slices.Contains(args, r) {
					t.Fatalf("args %v must not contain %q", args, r)
				}
			}
		})
	}
}
