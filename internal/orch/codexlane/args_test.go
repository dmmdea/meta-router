package codexlane

import (
	"strings"
	"testing"
)

func TestBuildArgsHardRules(t *testing.T) {
	tests := []struct {
		name    string
		req     RunReq
		wantErr string
		want    []string
	}{
		{name: "model required (pin -m always)", req: RunReq{Prompt: "x"}, wantErr: "model is required"},
		{name: "prompt positional, exec --json, sandbox default",
			req:  RunReq{Prompt: "do it", Model: "gpt-5.5"},
			want: []string{"exec", "--json", "--skip-git-repo-check", "--sandbox", "workspace-write", "-m", "gpt-5.5", "do it"}},
		{name: "effort via -c model_reasoning_effort (AMS run-codex-audit.ps1 pattern, R12)",
			req:  RunReq{Prompt: "x", Model: "gpt-5.5", Effort: "low"},
			want: []string{"-c", `model_reasoning_effort="low"`}},
		{name: "stdin prompt forbidden (codex hangs reading stdin under the orchestrator)",
			req: RunReq{Prompt: "x", Model: "gpt-5.5", Extra: []string{"-"}}, wantErr: "stdin"},
		{name: "danger-full-access forbidden (unattended dispatch)",
			req: RunReq{Prompt: "x", Model: "gpt-5.5", Sandbox: "danger-full-access"}, wantErr: "danger-full-access"},

		// A2R-#4: the sandbox guard must not be bypassable via the Extra
		// passthrough. Each of these vectors must be rejected by name.
		{name: "extra --sandbox two-token override rejected",
			req: RunReq{Prompt: "x", Model: "gpt-5.5", Extra: []string{"--sandbox", "danger-full-access"}}, wantErr: "sandbox"},
		{name: "extra --sandbox=danger-full-access single-token override rejected",
			req: RunReq{Prompt: "x", Model: "gpt-5.5", Extra: []string{"--sandbox=danger-full-access"}}, wantErr: "sandbox"},
		{name: "extra -s short sandbox override rejected",
			req: RunReq{Prompt: "x", Model: "gpt-5.5", Extra: []string{"-s", "danger-full-access"}}, wantErr: "sandbox"},
		{name: "extra --dangerously-bypass-approvals-and-sandbox rejected",
			req: RunReq{Prompt: "x", Model: "gpt-5.5", Extra: []string{"--dangerously-bypass-approvals-and-sandbox"}}, wantErr: "bypass"},
		{name: "extra -c sandbox_mode config override rejected",
			req: RunReq{Prompt: "x", Model: "gpt-5.5", Extra: []string{"-c", "sandbox_mode=danger-full-access"}}, wantErr: "sandbox_mode"},
		{name: "extra -c danger-full-access anywhere rejected",
			req: RunReq{Prompt: "x", Model: "gpt-5.5", Extra: []string{"-c", "sandbox_permissions=[\"danger-full-access\"]"}}, wantErr: "danger-full-access"},
		{name: "extra -c approval_policy config override rejected",
			req: RunReq{Prompt: "x", Model: "gpt-5.5", Extra: []string{"-c", "approval_policy=never"}}, wantErr: "approval_policy"},
		{name: "extra --full-auto rejected (defensive)",
			req: RunReq{Prompt: "x", Model: "gpt-5.5", Extra: []string{"--full-auto"}}, wantErr: "full-auto"},
		{name: "extra --yolo rejected (defensive)",
			req: RunReq{Prompt: "x", Model: "gpt-5.5", Extra: []string{"--yolo"}}, wantErr: "yolo"},
		{name: "extra --ask-for-approval override rejected (weakens approvals)",
			req: RunReq{Prompt: "x", Model: "gpt-5.5", Extra: []string{"--ask-for-approval", "never"}}, wantErr: "approval"},

		// A benign Extra passthrough (a harmless -c config key) is still allowed.
		{name: "benign extra -c passthrough allowed",
			req:  RunReq{Prompt: "x", Model: "gpt-5.5", Extra: []string{"-c", "model_reasoning_summary=none"}},
			want: []string{"-c", "model_reasoning_summary=none"}},
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
			if args[len(args)-1] != tc.req.Prompt {
				t.Fatalf("prompt must be the LAST positional arg: %v", args)
			}
		})
	}
}
