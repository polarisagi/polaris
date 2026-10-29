package marketplace

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/polarisagi/polaris/internal/extension/lifecycle"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/protocol/schema"
	"github.com/polarisagi/polaris/internal/security/network"
	"github.com/polarisagi/polaris/internal/store"
	"github.com/polarisagi/polaris/internal/store/repo"
	"github.com/polarisagi/polaris/pkg/apperr"
)

func TestInstallDir_MapsIdentityAndRejectsEscape(t *testing.T) {
	root := t.TempDir()
	got, err := InstallDir(root, "polarisagi/polaris-plugins-official/polaris-browser-bridge")
	want := filepath.Join(root, "installed", "polarisagi", "polaris-plugins-official", "polaris-browser-bridge")
	if err != nil || got != want {
		t.Fatalf("got %q %v, want %q", got, err, want)
	}
	if got, _ := InstallDir(root, "org/skills/skills/.curated/pdf"); got != filepath.Join(root, "installed", "org", "skills", "skills", ".curated", "pdf") {
		t.Fatalf("dot-prefixed segment: %q", got)
	}
	if got, _ := InstallDir(root, SourceIdentity("my tool")); got != filepath.Join(root, "installed", "_source", "my_tool") {
		t.Fatalf("unsafe chars must be replaced: %q", got)
	}
	for _, bad := range []string{"", "a/../b", "../x", "a//b", "a/.", "/abs"} {
		if _, err := InstallDir(root, bad); err == nil {
			t.Fatalf("identity %q accepted", bad)
		}
	}
}

func TestInstanceID_DeterministicPerIdentity(t *testing.T) {
	a := InstanceID("polaris-browser-bridge", "polarisagi/polaris-plugins-official/polaris-browser-bridge")
	if a != InstanceID("polaris-browser-bridge", "polarisagi/polaris-plugins-official/polaris-browser-bridge") {
		t.Fatal("same identity must yield same ID")
	}
	if a == InstanceID("polaris-browser-bridge", "other/mkt/polaris-browser-bridge") {
		t.Fatal("same name in another marketplace must yield a different ID")
	}
}

// ADR-0124：卸载重装后实例 ID 与安装路径不变；卸载未完成时拒绝重装；上一代残留文件被清掉。
func TestCatalog_ReinstallKeepsIDAndPath(t *testing.T) {
	ctx := context.Background()
	s, err := store.OpenSQLite(filepath.Join(t.TempDir(), "p.db"), schema.FS)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	extRepo := repo.NewSQLiteExtensionRepository(s.DB())
	data := t.TempDir()
	mkt := filepath.Join(t.TempDir(), "mkt")
	writeTree(t, mkt, map[string]string{
		".claude-plugin/marketplace.json":        `{"name":"team","owner":{"name":"T"},"plugins":[{"name":"app","source":"./plugins/app","version":"1.0.0"}]}`,
		"plugins/app/.claude-plugin/plugin.json": `{"name":"app"}`,
		"plugins/app/skills/a/SKILL.md":          "---\nname: a\ndescription: d\n---\nx",
	})
	sync := NewCatalogSync(extRepo, nil, network.NewSafeHTTPClient(nil), filepath.Join(data, "cache"))
	if _, err := sync.Sync(ctx, protocol.Marketplace{ID: "org/team", Type: "plugin", RepoURL: mkt, TrustTier: 2}, false); err != nil {
		t.Fatal(err)
	}
	fsm := lifecycle.NewInstallFSM(extRepo)
	fsm.RegisterInstaller(lifecycle.NewPluginInstaller(extRepo, nopConnector{}, nil))
	mgr := NewManager(extRepo, nil, &mockPolicyGate{allowed: true}, &mockPrefs{}, nil, nil, &mockOutbox{}).WithInstallFSM(fsm)
	extDir := filepath.Join(data, "extensions")
	inst := NewCatalogInstaller(mgr, extRepo, NewSourceFetcher(network.NewSafeHTTPClient(nil), t.TempDir()), sync, extDir)

	first, err := inst.Install(ctx, CatalogInstallRequest{CatalogID: "org/team/app", Principal: "user"})
	if err != nil {
		t.Fatal(err)
	}
	wantDir := filepath.Join(extDir, "installed", "org", "team", "app")
	row, _ := extRepo.GetInstance(ctx, first)
	if first != InstanceID("app", "org/team/app") || row == nil || row.InstallPath != wantDir || row.InstalledVersion != "1.0.0" {
		t.Fatalf("first install: id=%s row=%+v", first, row)
	}

	if err := mgr.UninstallExtension(ctx, "org/team/app"); err != nil {
		t.Fatal(err)
	}
	if _, err := inst.Install(ctx, CatalogInstallRequest{CatalogID: "org/team/app", Principal: "user"}); apperr.CodeOf(err) != apperr.CodeConflict {
		t.Fatalf("reinstall while uninstalling must conflict: %v", err)
	}

	// 模拟卸载处理器完成：清运行时与实例行，但文件删除失败留下残留。
	if err := fsm.UninstallRuntime(ctx, "plugin", first, row.RuntimeID); err != nil {
		t.Fatal(err)
	}
	if err := extRepo.DeleteInstance(ctx, first); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(wantDir, "stale.txt")
	if err := os.WriteFile(stale, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	second, err := inst.Install(ctx, CatalogInstallRequest{CatalogID: "org/team/app", Principal: "user"})
	if err != nil {
		t.Fatal(err)
	}
	row, _ = extRepo.GetInstance(ctx, second)
	if second != first || row == nil || row.InstallPath != wantDir || row.Status != "installed" || row.InstalledVersion != "1.0.0" {
		t.Fatalf("reinstall: id=%s row=%+v", second, row)
	}
	// 原地升级同样回写版本：模拟已装版本落后于目录。
	if err := extRepo.UpdateInstanceInstalledVersion(ctx, second, "0.9.0"); err != nil {
		t.Fatal(err)
	}
	if err := inst.Upgrade(ctx, CatalogInstallRequest{CatalogID: "org/team/app", ExtensionID: second, Principal: "user"}); err != nil {
		t.Fatal(err)
	}
	if row, _ = extRepo.GetInstance(ctx, second); row == nil || row.InstalledVersion != "1.0.0" || row.InstallPath != wantDir {
		t.Fatalf("upgrade: row=%+v", row)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("leftover from previous generation must be removed: %v", err)
	}
}
