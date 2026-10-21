package read_file

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
)

// 现场复现（ADR-0121）：Decide 放行后（凭据已下发），read_file 读可信根外的 go.mod 必须成功；
// 无凭据的兜底路径仍按白名单拒绝。
func TestReadFile_GrantLetsOutsideRootRead(t *testing.T) {
	trusted := t.TempDir()
	outside := t.TempDir()
	gomod := filepath.Join(outside, "go.mod")
	if err := os.WriteFile(gomod, []byte("module example.com/x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fn := MakeReadFileFn([]string{trusted})
	in := []byte(`{"path":"` + gomod + `"}`)

	if _, err := fn(context.Background(), in); err == nil {
		t.Fatal("无凭据时读可信根外应拒绝")
	}
	for _, g := range []protocol.PermissionGrant{protocol.GrantAuto, protocol.GrantExplicit} {
		out, err := fn(protocol.WithPermissionGrant(context.Background(), g), in)
		if err != nil {
			t.Fatalf("grant=%d 应成功: %v", g, err)
		}
		if len(out) == 0 {
			t.Fatal("空输出")
		}
	}
}
