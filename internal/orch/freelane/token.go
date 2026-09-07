package freelane

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrUnconfigured: no credential file exists for the lane. A TYPED absence —
// the lane routes as "unconfigured" (masked) and `run` defers with the path to
// provision, never a stack trace and never a fallback to an env var.
var ErrUnconfigured = errors.New("free lane unconfigured: no credential file")

// TokenPath is <stateDir>/free/<lane>.token — the glm-token pattern (a file
// the operator writes once, 0600), one per provider so the accounts never
// mix. Ambient env (GROQ_API_KEY and friends) is deliberately NOT consulted:
// the orchestrator's standing rule is that a credential's identity is explicit
// config, because the operator's shell may carry keys for other businesses.
func TokenPath(stateDir, lane string) string {
	return filepath.Join(stateDir, "free", lane+".token")
}

// LoadToken reads and trims the lane's credential file. Absent → ErrUnconfigured
// (wrapped, with the path). Present but empty → an error naming the path.
func LoadToken(stateDir, lane string) (string, error) {
	p := TokenPath(stateDir, lane)
	b, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("%w: %s (write the provider API key there, one line, 0600)", ErrUnconfigured, p)
		}
		return "", fmt.Errorf("free lane %s: credential file %s unreadable: %w", lane, p, err)
	}
	tok := cleanToken(string(b))
	if tok == "" {
		return "", fmt.Errorf("free lane %s: credential file %s is empty", lane, p)
	}
	return tok, nil
}

// utf8BOM is what a Windows editor (Notepad "UTF-8", PowerShell 5.1
// `Set-Content -Encoding UTF8`, `Out-File -Encoding utf8`) puts at the front of
// a hand-created file. It is NOT whitespace, so TrimSpace leaves it in place
// and it rides into the Authorization header — every local check passes (the
// file exists, the token is non-empty, the dry-run prints an endpoint) and the
// dispatch fails at the vendor with a bare 401, which reads as a bad key. The
// operator provisions these files by hand on two machines, so the loader
// tolerates the byte rather than teaching everyone one editor's quirk.
const utf8BOM = "\ufeff"

// cleanToken strips a leading BOM and surrounding whitespace/newlines.
func cleanToken(raw string) string {
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(raw), utf8BOM))
}
