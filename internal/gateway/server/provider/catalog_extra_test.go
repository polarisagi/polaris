package provider

import (
	"github.com/polarisagi/polaris/internal/store/repo"

	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

func TestHandleListCatalogProviders(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
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

func TestGetFallbackGeneralModel(t *testing.T) {
	models := []catalogModelRow{
		{modelID: "gpt-4o-mini", displayName: "GPT-4o Mini", recommendedRole: "reasoning"},
	}
	model := getFallbackGeneralModel(models)
	if model.modelID == "" {
		t.Errorf("expected a fallback model")
	}
}
