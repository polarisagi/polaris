package glob

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
)

func TestGlobDoesNotReturnSymlinkTargetsOutsideReadableRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "inside.txt"), []byte("inside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	fn := MakeGlobFn([]string{root})
	for _, pattern := range []string{"link/*.txt", "**/*.txt"} {
		out, err := fn(context.Background(), mustJSON(t, map[string]string{"pattern": pattern}))
		if err != nil {
			t.Fatalf("glob %q: %v", pattern, err)
		}
		var result struct {
			Matches []string `json:"matches"`
		}
		if err := json.Unmarshal(out, &result); err != nil {
			t.Fatal(err)
		}
		for _, match := range result.Matches {
			if match == filepath.Join(root, "link", "secret.txt") || match == filepath.Join(outside, "secret.txt") {
				t.Fatalf("glob exposed path through symlink %q: %v", pattern, result.Matches)
			}
		}
		if pattern == "**/*.txt" && len(result.Matches) != 1 {
			t.Fatalf("ordinary root match regressed: %v", result.Matches)
		}
	}

	ctx := protocol.WithConfinedRoot(context.Background(), root)
	ctx = protocol.WithPermissionGrant(ctx, protocol.GrantExplicit)
	out, err := fn(ctx, mustJSON(t, map[string]string{"pattern": "link/*.txt"}))
	if err != nil {
		t.Fatal(err)
	}
	var confined struct {
		Matches []string `json:"matches"`
	}
	if err := json.Unmarshal(out, &confined); err != nil {
		t.Fatal(err)
	}
	if len(confined.Matches) != 0 {
		t.Fatalf("explicit grant widened confined root: %v", confined.Matches)
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
