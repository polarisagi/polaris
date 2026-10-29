package plugin

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http/httptest"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/polarisagi/polaris/internal/extension/marketplace"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/protocol/schema"
	"github.com/polarisagi/polaris/internal/store/repo"
	"github.com/polarisagi/polaris/pkg/types"
)

func TestApplyCatalogOverlay(t *testing.T) {
	entries := []protocol.RegistryEntry{
		{ID: "mp/fresh", Name: "fresh", Description: "same"},
		{ID: "mp/stale", Name: "stale", Description: "changed upstream"},
		{ID: "mp/empty", Name: "empty", Description: ""},
		{ID: "mp/none", Name: "none", Description: "x"},
	}
	overlay := map[string]types.ExtCatalogOverlayRow{
		"mp/fresh": {SrcHash: marketplace.OverlaySourceHash("fresh", "same"), NameZh: "新", Topics: `["code"]`, Featured: 3},
		"mp/stale": {SrcHash: marketplace.OverlaySourceHash("stale", "old text"), NameZh: "旧"},
		"mp/empty": {SrcHash: marketplace.OverlaySourceHash("empty", ""), DescriptionEn: "filled from manifest"},
	}
	applyCatalogOverlay(entries, overlay)
	if entries[0].I18nStale || entries[0].NameZh != "新" || len(entries[0].Topics) != 1 || entries[0].Featured != 3 {
		t.Fatalf("fresh entry wrong: %+v", entries[0])
	}
	if !entries[1].I18nStale || entries[1].NameZh != "旧" {
		t.Fatalf("stale entry must be marked stale but keep zh: %+v", entries[1])
	}
	if entries[2].I18nStale || entries[2].Description != "filled from manifest" {
		t.Fatalf("empty description must be filled and not stale: %+v", entries[2])
	}
	if entries[3].NameZh != "" || entries[3].I18nStale {
		t.Fatalf("entry without overlay must be untouched: %+v", entries[3])
	}
}

func TestHandleListPluginCatalog_WithOverlay(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	for _, f := range []string{"019_extension_catalog.sql", "020_extension_instances.sql"} {
		ddl, err := schema.FS.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(string(ddl)); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
	}
	if _, err := db.Exec(`CREATE TABLE plugin_marketplaces (id TEXT PRIMARY KEY, sort_order INTEGER)`); err != nil {
		t.Fatal(err)
	}
	extRepo := repo.NewSQLiteExtensionRepository(db)
	ctx := context.Background()
	mk := func(id, name string) types.ExtCatalogRow {
		payload, _ := json.Marshal(protocol.RegistryEntry{ID: id, Type: "plugin", Name: name, Description: "d"})
		return types.ExtCatalogRow{ID: id, MarketplaceID: "mp", Type: "plugin", Name: name, Description: "d", Payload: string(payload)}
	}
	if _, err := extRepo.ReplaceMarketplaceCatalog(ctx, "mp", []types.ExtCatalogRow{mk("mp/a", "a"), mk("mp/b", "b")}); err != nil {
		t.Fatal(err)
	}
	err = extRepo.ReplaceCatalogOverlay(ctx, []types.ExtCatalogOverlayRow{
		{CatalogID: "mp/b", SrcHash: marketplace.OverlaySourceHash("b", "d"), NameZh: "乙", Topics: `["code"]`,
			Departments: `[]`, Industries: `[]`, Life: `[]`, Featured: 10},
	}, map[string]string{marketplace.OverlayMetaTaxonomy: `{"version":1,"values":{"topic":{"code":{"en":"Code","zh":"代码"}}}}`})
	if err != nil {
		t.Fatal(err)
	}

	h := &PluginHandler{DB: db, ExtRepo: extRepo}
	w := httptest.NewRecorder()
	h.HandleListPluginCatalog(w, httptest.NewRequest("GET", "/v1/plugins/catalog", nil))
	var resp struct {
		Catalog  []protocol.RegistryEntry `json:"catalog"`
		Taxonomy json.RawMessage          `json:"taxonomy"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Catalog) != 2 || resp.Catalog[0].ID != "mp/b" || resp.Catalog[0].NameZh != "乙" {
		t.Fatalf("featured entry must sort first with zh name: %+v", resp.Catalog)
	}
	if len(resp.Taxonomy) == 0 {
		t.Fatal("taxonomy must be returned")
	}
}

// 已装版本取 extension_instances.installed_version：与目录版本不同、或历史安装为空，都标记可升级并带实例 ID。
func TestHandleListPluginCatalog_UpdateAvailable(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	for _, f := range []string{"019_extension_catalog.sql", "020_extension_instances.sql"} {
		ddl, err := schema.FS.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(string(ddl)); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
	}
	if _, err := db.Exec(`CREATE TABLE plugin_marketplaces (id TEXT PRIMARY KEY, sort_order INTEGER)`); err != nil {
		t.Fatal(err)
	}
	extRepo := repo.NewSQLiteExtensionRepository(db)
	ctx := context.Background()
	mk := func(name, version string) types.ExtCatalogRow {
		payload, _ := json.Marshal(protocol.RegistryEntry{ID: "mp/" + name, Type: "plugin", Name: name, Version: version})
		return types.ExtCatalogRow{ID: "mp/" + name, MarketplaceID: "mp", Type: "plugin", Name: name, Version: version, Payload: string(payload)}
	}
	if _, err := extRepo.ReplaceMarketplaceCatalog(ctx, "mp", []types.ExtCatalogRow{
		mk("same", "v2"), mk("old", "v2"), mk("legacy", "v2"), mk("absent", "v2")}); err != nil {
		t.Fatal(err)
	}
	for _, r := range []types.ExtInstanceRow{
		{ID: "ext_same", CatalogID: "mp/same", InstalledVersion: "v2"},
		{ID: "ext_old", CatalogID: "mp/old", InstalledVersion: "v1"},
		{ID: "ext_legacy", CatalogID: "mp/legacy"},
	} {
		r.ExtType, r.Origin, r.Name, r.Status, r.Config = "plugin", "marketplace", r.ID, "installed", "{}"
		if err := extRepo.UpsertInstance(ctx, r); err != nil {
			t.Fatal(err)
		}
	}

	h := &PluginHandler{DB: db, ExtRepo: extRepo}
	w := httptest.NewRecorder()
	h.HandleListPluginCatalog(w, httptest.NewRequest("GET", "/v1/plugins/catalog", nil))
	var resp struct {
		Catalog []protocol.RegistryEntry `json:"catalog"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	got := map[string]protocol.RegistryEntry{}
	for _, e := range resp.Catalog {
		got[e.Name] = e
	}
	if e := got["same"]; !e.Installed || e.UpdateAvailable || e.InstanceID != "ext_same" {
		t.Fatalf("same: %+v", e)
	}
	if e := got["old"]; !e.UpdateAvailable || e.InstalledVersion != "v1" || e.InstanceID != "ext_old" {
		t.Fatalf("old: %+v", e)
	}
	if e := got["legacy"]; !e.UpdateAvailable || e.InstanceID != "ext_legacy" {
		t.Fatalf("legacy without installed_version must be upgradable: %+v", e)
	}
	if e := got["absent"]; e.Installed || e.UpdateAvailable || e.InstanceID != "" {
		t.Fatalf("absent: %+v", e)
	}
}
