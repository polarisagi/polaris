package repo

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/polarisagi/polaris/internal/protocol/schema"
	"github.com/polarisagi/polaris/pkg/types"
)

func TestCatalogOverlayRepo(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	// :memory: 每条连接都是独立空库，必须单连接。
	db.SetMaxOpenConns(1)
	defer db.Close()
	ddl, err := schema.FS.ReadFile("019_extension_catalog.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(ddl)); err != nil {
		t.Fatal(err)
	}
	r := NewSQLiteExtensionRepository(db)
	ctx := context.Background()

	if v, err := r.GetCatalogOverlayMeta(ctx, "etag"); err != nil || v != "" {
		t.Fatalf("missing meta must be empty: %q %v", v, err)
	}
	first := []types.ExtCatalogOverlayRow{
		{CatalogID: "mp/a", SrcHash: "h1", NameZh: "甲", Topics: `["code"]`, Departments: `[]`, Industries: `[]`, Life: `[]`},
		{CatalogID: "mp/b", SrcHash: "h2", Topics: `[]`, Departments: `[]`, Industries: `[]`, Life: `[]`, Featured: 5},
	}
	if err := r.ReplaceCatalogOverlay(ctx, first, map[string]string{"etag": `"v1"`}); err != nil {
		t.Fatal(err)
	}
	second := []types.ExtCatalogOverlayRow{
		{CatalogID: "mp/c", SrcHash: "h3", Topics: `["databases"]`, Departments: `[]`, Industries: `[]`, Life: `[]`},
	}
	if err := r.ReplaceCatalogOverlay(ctx, second, map[string]string{"etag": `"v2"`, "taxonomy": "{}"}); err != nil {
		t.Fatal(err)
	}
	got, err := r.ListCatalogOverlay(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got["mp/c"].Topics != `["databases"]` {
		t.Fatalf("replace must drop old rows, got %+v", got)
	}
	if v, _ := r.GetCatalogOverlayMeta(ctx, "etag"); v != `"v2"` {
		t.Fatalf("meta must be upserted, got %q", v)
	}
}
