package marketplace

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
)

// GR-8-003：远端元数据中的 ID/Command 不得让安装目录逃逸 baseInstallDir；
// 尤其 ID==".." 时原实现会 RemoveAll 基目录的父目录。
func TestInstall_RejectsEscapingPackageID(t *testing.T) {
	parent := t.TempDir()
	base := filepath.Join(parent, "plugins")
	sentinel := filepath.Join(parent, "keep.txt")
	if err := os.WriteFile(sentinel, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := &MCPMarketplaceClient{baseInstallDir: base}
	for _, id := range []string{"..", ".", ""} {
		if _, err := c.Install(context.Background(), protocol.RegistryEntry{ID: id, Command: "srv", Transport: "stdio"}); err == nil {
			t.Fatalf("id %q accepted", id)
		}
	}
	if _, err := c.Install(context.Background(), protocol.RegistryEntry{ID: "ok", Command: "../../evil", Transport: "stdio", URL: "https://x/y"}); err == nil {
		t.Fatal("escaping command accepted")
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("parent directory content removed: %v", err)
	}
}
