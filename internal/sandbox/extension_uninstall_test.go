package sandbox

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol/schema"
	"github.com/polarisagi/polaris/internal/store"
	"github.com/polarisagi/polaris/internal/store/repo"
	"github.com/polarisagi/polaris/pkg/types"
)

type countingRuntime struct{ calls int }

func (c *countingRuntime) UninstallRuntime(context.Context, string, string, string) error {
	c.calls++
	return nil
}

// ADR-0124：实例 ID 与路径按身份复用，旧代次的卸载消息不得删掉重装后的同名插件。
func TestExtensionUninstall_SkipsStaleGeneration(t *testing.T) {
	ctx := context.Background()
	s, err := store.OpenSQLite(filepath.Join(t.TempDir(), "p.db"), schema.FS)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	extRepo := repo.NewSQLiteExtensionRepository(s.DB())
	dir := filepath.Join(t.TempDir(), "installed", "org", "team", "app")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	row := types.ExtInstanceRow{ID: "ext_app_1", ExtType: "plugin", Origin: "marketplace", Name: "app",
		InstallPath: dir, Status: "installed", CreatedAt: "2026-10-28T10:00:00Z"}
	if err := extRepo.UpsertInstance(ctx, row); err != nil {
		t.Fatal(err)
	}
	rt := &countingRuntime{}
	h := NewExtensionUninstallHandler(rt, extRepo, 5)
	handle := func(gen string) error {
		p, _ := json.Marshal(ExtensionUninstallPayload{InstanceID: row.ID, InstallPath: dir, ExtType: "plugin", Generation: gen})
		return h.Handle(ctx, &store.OutboxRecord{Payload: p})
	}

	if err := handle("2026-10-27T09:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if inst, _ := extRepo.GetInstance(ctx, row.ID); rt.calls != 0 || inst == nil {
		t.Fatalf("stale generation must not touch runtime or instance: calls=%d inst=%v", rt.calls, inst)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("stale generation must not remove files: %v", err)
	}

	if err := handle(row.CreatedAt); err != nil {
		t.Fatal(err)
	}
	if inst, _ := extRepo.GetInstance(ctx, row.ID); rt.calls != 1 || inst != nil {
		t.Fatalf("current generation must uninstall: calls=%d inst=%v", rt.calls, inst)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("current generation must remove files: %v", err)
	}
	// 重试：实例已删，视为已完成。
	if err := handle(row.CreatedAt); err != nil || rt.calls != 1 {
		t.Fatalf("retry after completion: calls=%d err=%v", rt.calls, err)
	}
}
