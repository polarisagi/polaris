package marketplace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/extension/lifecycle"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/protocol/schema"
	"github.com/polarisagi/polaris/internal/security/network"
	"github.com/polarisagi/polaris/internal/store"
	"github.com/polarisagi/polaris/internal/store/repo"
	"github.com/polarisagi/polaris/pkg/types"
)

type memSkills struct{ names []string }

func (m *memSkills) Register(_ context.Context, meta types.SkillMeta) error {
	m.names = append(m.names, meta.Name)
	return nil
}
func (m *memSkills) Get(context.Context, string, string) (*types.SkillMeta, error) { return nil, nil }
func (m *memSkills) List(context.Context, types.SkillFilter) ([]types.SkillMeta, error) {
	return nil, nil
}
func (m *memSkills) Deprecate(context.Context, string, string, string) error { return nil }

type nopConnector struct{}

func (nopConnector) StartFromDB(context.Context, string) error { return nil }
func (nopConnector) GetClient(string) protocol.MCPClient       { return nil }
func (nopConnector) Remove(string)                             {}

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCatalog_SyncAndInstallWithDependencies(t *testing.T) {
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
		".claude-plugin/marketplace.json": `{"name":"team","owner":{"name":"T"},"plugins":[
		  {"name":"app","source":"./plugins/app"},
		  {"name":"lib","source":"./plugins/lib","version":"1.0.0"},
		  {"name":"loop-a","source":"./plugins/loop-a"},
		  {"name":"loop-b","source":"./plugins/loop-b"},
		  {"name":"far","source":"./plugins/far"}]}`,
		"plugins/app/.claude-plugin/plugin.json":    `{"name":"app","dependencies":["lib"]}`,
		"plugins/app/skills/a/SKILL.md":             "---\nname: a\ndescription: d\n---\nx",
		"plugins/lib/.claude-plugin/plugin.json":    `{"name":"lib","version":"1.0.0"}`,
		"plugins/lib/skills/l/SKILL.md":             "---\nname: l\ndescription: d\n---\nx",
		"plugins/loop-a/.claude-plugin/plugin.json": `{"name":"loop-a","dependencies":["loop-b"]}`,
		"plugins/loop-b/.claude-plugin/plugin.json": `{"name":"loop-b","dependencies":["loop-a"]}`,
		"plugins/far/.claude-plugin/plugin.json":    `{"name":"far","dependencies":[{"name":"x","marketplace":"other"}]}`,
		"plugins/far/skills/f/SKILL.md":             "---\nname: f\ndescription: d\n---\nx",
	})
	skills := filepath.Join(t.TempDir(), "skills")
	writeTree(t, skills, map[string]string{"skills/.curated/pdf/SKILL.md": "---\nname: pdf\ndescription: PDF tools\n---\nbody"})

	sync := NewCatalogSync(extRepo, nil, network.NewSafeHTTPClient(nil), filepath.Join(data, "cache"))
	if rows, err := sync.Sync(ctx, protocol.Marketplace{ID: "org/team", Type: "plugin", RepoURL: mkt, TrustTier: 2}, false); err != nil || len(rows) != 5 {
		t.Fatalf("plugin sync: %d %v", len(rows), err)
	}
	if rows, err := sync.Sync(ctx, protocol.Marketplace{ID: "org/skills", Type: "skill", RepoURL: skills, TrustTier: 2}, false); err != nil ||
		len(rows) != 1 || rows[0].ID != "org/skills/skills/.curated/pdf" {
		t.Fatalf("skill sync: %+v %v", rows, err)
	}

	fsm := lifecycle.NewInstallFSM(extRepo)
	fsm.RegisterInstaller(lifecycle.NewPluginInstaller(extRepo, nopConnector{}, nil))
	skillReg := &memSkills{}
	fsm.RegisterInstaller(lifecycle.NewSkillInstaller(extRepo, skillReg))
	mgr := NewManager(extRepo, nil, &mockPolicyGate{allowed: true}, &mockPrefs{}, nil, nil, nil).WithInstallFSM(fsm)
	inst := NewCatalogInstaller(mgr, extRepo, NewSourceFetcher(network.NewSafeHTTPClient(nil), t.TempDir()), sync, filepath.Join(data, "extensions"))

	if _, err := inst.Install(ctx, CatalogInstallRequest{CatalogID: "org/team/app", Principal: "user"}); err != nil {
		t.Fatal(err)
	}
	enabled := map[string]bool{}
	plugins, _ := extRepo.ListPlugins(ctx)
	for _, p := range plugins {
		enabled[p.Name] = p.Enabled
	}
	if !enabled["lib"] || !enabled["app"] {
		t.Fatalf("dependency must be installed first and both enabled: %v", enabled)
	}
	if _, err := inst.Install(ctx, CatalogInstallRequest{CatalogID: "org/team/loop-a", Principal: "user"}); err == nil ||
		!strings.Contains(err.Error(), "circular") {
		t.Fatalf("circular dependency must fail: %v", err)
	}
	if _, err := inst.Install(ctx, CatalogInstallRequest{CatalogID: "org/team/far", Principal: "user"}); err != nil {
		t.Fatal(err)
	}
	plugins, _ = extRepo.ListPlugins(ctx)
	for _, p := range plugins {
		if p.Name == "far" && p.Enabled {
			t.Fatal("unresolvable cross-marketplace dependency: installed but disabled")
		}
	}
	if _, err := inst.Install(ctx, CatalogInstallRequest{CatalogID: "org/skills/skills/.curated/pdf", Principal: "user"}); err != nil {
		t.Fatalf("skill install: %v", err)
	}
	if len(skillReg.names) != 1 || skillReg.names[0] != "skill:pdf" {
		t.Fatalf("skill registered: %v", skillReg.names)
	}
}
