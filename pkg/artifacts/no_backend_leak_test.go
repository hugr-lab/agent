package artifacts_test

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestManagerNoBackendConfigLeak enforces the manager-side
// invariant from contracts/manager.md §Invariants:
//
//	Manager NEVER reads cfg.Artifacts.FS.Dir or any other backend-
//	specific key. All bytes go through Storage.Put / Open / LocalPath.
//
// Implemented as a grep over pkg/artifacts/*.go (excluding the
// storage subpackage where backend-specific code legitimately lives).
// Encoded as a test rather than a code-review note so the invariant
// can't drift silently.
func TestManagerNoBackendConfigLeak(t *testing.T) {
	// Match field access (`cfg.Artifacts.FS.Dir`, `cfg.S3.Bucket`,
	// etc.) — trailing dot anchors against accidental code, not
	// documentation. Test files and doc.go are excluded because
	// they discuss the rule by name.
	cmd := exec.Command("grep", "-RIEn",
		"--include=*.go",
		"--exclude=*_test.go",
		"--exclude=doc.go",
		"--exclude-dir=storage",
		"--exclude-dir=store",
		"-e", `cfg\.Artifacts\.FS\.`,
		"-e", `cfg\.Artifacts\.S3\.`,
		"-e", `cfg\.FS\.`,
		"-e", `cfg\.S3\.`,
		".")
	cmd.Dir = "."
	out, err := cmd.Output()
	// grep exits with 1 when there are no matches — that's the
	// expected case. Any other non-zero exit is a real failure.
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return // no matches, pass.
		}
		t.Fatalf("grep failed: %v", err)
	}
	hits := strings.TrimSpace(string(out))
	assert.Empty(t, hits,
		"manager package must not read backend-specific config keys "+
			"(use Storage.Put/Open/LocalPath instead). Hits:\n%s", hits)
}
