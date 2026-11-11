package grep

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
)

func TestGrepDoesNotReadSymlinkTargetsOutsideReadableRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "inside.txt"), []byte("needle inside\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("needle outside\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(root, "link.txt")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	fn := MakeGrepFn([]string{root})
	for _, ctx := range []context.Context{
		context.Background(),
		protocol.WithPermissionGrant(protocol.WithConfinedRoot(context.Background(), root), protocol.GrantExplicit),
	} {
		out, err := fn(ctx, mustJSON(t, map[string]string{"pattern": "needle", "path": root, "output_mode": "content"}))
		if err != nil {
			t.Fatal(err)
		}
		var result struct {
			Matches []struct {
				File string `json:"file"`
				Text string `json:"text"`
			} `json:"matches"`
		}
		if err := json.Unmarshal(out, &result); err != nil {
			t.Fatal(err)
		}
		if len(result.Matches) != 1 || result.Matches[0].File != filepath.Join(root, "inside.txt") || result.Matches[0].Text != "needle inside" {
			t.Fatalf("unexpected grep result: %+v", result.Matches)
		}
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
