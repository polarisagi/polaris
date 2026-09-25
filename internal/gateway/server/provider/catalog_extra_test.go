package provider

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/polarisagi/polaris/internal/store/repo"
	"github.com/polarisagi/polaris/pkg/types"
)

func TestHandleListCatalogProviders(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	// :memory: 每条连接都是独立空库（无 cache=shared），池开出第二条即读到空表。
	db.SetMaxOpenConns(1)
	defer db.Close()

	_, err = db.Exec("CREATE TABLE IF NOT EXISTS sys_providers (id TEXT, display_name TEXT, provider_type TEXT, is_local INTEGER, default_base_url TEXT, homepage TEXT, created_at DATETIME, display_order INTEGER)")
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec("CREATE TABLE IF NOT EXISTS sys_provider_models (id TEXT, catalog_provider_id TEXT, model_id TEXT, display_name TEXT, recommended_role TEXT, display_order INTEGER)")
	if err != nil {
		t.Fatal(err)
	}

	h := &ProviderHandler{DB: db, ExtRepo: repo.NewSQLiteExtensionRepository(db), ProviderRepo: repo.NewSQLiteProviderRepository(db)}

	req := httptest.NewRequest("GET", "/api/v1/catalog/providers", nil)
	w := httptest.NewRecorder()
	h.HandleListCatalogProviders(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Errorf("list catalog providers failed")
	}
}

func TestAssignCatalogRoles(t *testing.T) {
	models := []catalogModelRow{
		{modelID: "flash", displayName: "Flash", recommendedRole: "default"},
		{modelID: "pro", displayName: "Pro", recommendedRole: "reasoning"},
	}
	roles := func(rows []types.ProviderModelRow) []string {
		out := make([]string, 0, len(rows))
		for _, r := range rows {
			out = append(out, r.ModelID+"="+r.Role)
		}
		return out
	}

	// 空缺：按推荐角色落位，通用不落行（路由层派生为对话模型）
	got := roles(assignCatalogRoles(models, map[string]bool{}, "prov_x", "now"))
	if want := []string{"flash=default", "pro=reasoning"}; !slices.Equal(got, want) {
		t.Fatalf("vacant: got %v want %v", got, want)
	}

	// 已占用：不抢占，降为 general 备用
	got = roles(assignCatalogRoles(models, map[string]bool{"default": true}, "prov_x", "now"))
	if want := []string{"flash=general", "pro=reasoning"}; !slices.Equal(got, want) {
		t.Fatalf("default held: got %v want %v", got, want)
	}
}
