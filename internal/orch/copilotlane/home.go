package copilotlane

import (
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// EnsureHome provisions a per-run COPILOT_HOME under
// baseDir/copilot-home/<nano>. Unlike codexlane.EnsureHome there is nothing
// to seed: auth rides in COPILOT_GITHUB_TOKEN (first in the CLI's documented
// env precedence), and an EMPTY home is the point — the CLI otherwise loads
// the operator's desktop ~/.copilot config and spawns its MCP servers inside
// an orchestrated dispatch (live-verified 2026-09-01). Per-run isolation also
// prevents cross-dispatch session bleed (--resume/--continue have nothing to
// find).
func EnsureHome(baseDir string) (home string, cleanup func(), err error) {
	home = filepath.Join(baseDir, "copilot-home", strconv.FormatInt(time.Now().UnixNano(), 10))
	if err := os.MkdirAll(home, 0o700); err != nil {
		return "", nil, err
	}
	return home, func() { _ = os.RemoveAll(home) }, nil
}
