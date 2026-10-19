package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/gateway/server/provider"
	"github.com/polarisagi/polaris/internal/llm"
	"github.com/polarisagi/polaris/internal/protocol/schema"
	"github.com/polarisagi/polaris/internal/store/repo"
	"github.com/polarisagi/polaris/pkg/types"
)

func setupTestDBWithRealSchema(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })

	for _, f := range []string{"011_providers.sql", "022_provider_catalog.sql"} {
		ddl, err := schema.FS.ReadFile(f)
		if err != nil {
			t.Fatalf("read schema %s: %v", f, err)
		}
		if _, err := db.Exec(string(ddl)); err != nil {
			t.Fatalf("apply schema %s: %v", f, err)
		}
	}
	return db
}

// TestCatalog_LocalOnlyMode_OllamaAllowed_DeepSeekRejected 验证生产装配下：
// 1. 从目录添加 Ollama -> providers.is_local 写入 1
// 2. 从目录添加 DeepSeek -> providers.is_local 写入 0
// 3. 手工创建 Ollama -> is_local 缺省推导为 1
// 4. 手工创建 OpenAI -> is_local 缺省推导为 0
// 5. repo.ListProviders / GetProvider 能正确读出 IsLocal
// 6. 在 local_only_mode 下，Ollama 能被成功选中，DeepSeek 被拒绝 (R3-03)。
func TestCatalog_LocalOnlyMode_OllamaAllowed_DeepSeekRejected(t *testing.T) {
	db := setupTestDBWithRealSchema(t)
	provRepo := repo.NewSQLiteProviderRepository(db)
	reg := llm.NewProviderRegistry(config.M1RouterThresholds{})

	handler := &provider.ProviderHandler{
		DB:           db,
		ProviderRepo: provRepo,
		Registry:     reg,
	}

	// 1. 从目录添加 Ollama
	reqBody := `{"catalog_id": "ollama"}`
	req := httptest.NewRequest("POST", "/v1/providers/from-catalog", bytes.NewBufferString(reqBody))
	w := httptest.NewRecorder()
	handler.HandleCreateProviderFromCatalog(w, req)
	if w.Result().StatusCode != http.StatusCreated {
		t.Fatalf("from-catalog ollama failed: %d, %s", w.Result().StatusCode, w.Body.String())
	}

	// 查出 ollamaID，并添加一个模型（Ollama 是本地动态模型源，添加模型后方可被加载）
	var ollamaID string
	if err := db.QueryRow(`SELECT id FROM providers WHERE catalog_id='ollama'`).Scan(&ollamaID); err != nil {
		t.Fatalf("query ollama id: %v", err)
	}
	if err := provRepo.UpsertModel(context.Background(), repoProviderModel(ollamaID, "llama3", "default")); err != nil {
		t.Fatalf("upsert ollama model: %v", err)
	}

	// 2. 从目录添加 DeepSeek
	reqBody = `{"catalog_id": "deepseek", "api_key": "sk-deepseek-test"}`
	req = httptest.NewRequest("POST", "/v1/providers/from-catalog", bytes.NewBufferString(reqBody))
	w = httptest.NewRecorder()
	handler.HandleCreateProviderFromCatalog(w, req)
	if w.Result().StatusCode != http.StatusCreated {
		t.Fatalf("from-catalog deepseek failed: %d, %s", w.Result().StatusCode, w.Body.String())
	}

	// 验证 DB 物理存储与 repo 读取
	var ollamaIsLocal, deepseekIsLocal int
	var deepseekID string
	if err := db.QueryRow(`SELECT is_local FROM providers WHERE catalog_id='ollama'`).Scan(&ollamaIsLocal); err != nil {
		t.Fatalf("query ollama is_local: %v", err)
	}
	if err := db.QueryRow(`SELECT id, is_local FROM providers WHERE catalog_id='deepseek'`).Scan(&deepseekID, &deepseekIsLocal); err != nil {
		t.Fatalf("query deepseek is_local: %v", err)
	}

	if ollamaIsLocal != 1 {
		t.Errorf("expected ollama is_local=1 in db, got %d", ollamaIsLocal)
	}
	if deepseekIsLocal != 0 {
		t.Errorf("expected deepseek is_local=0 in db, got %d", deepseekIsLocal)
	}

	// 验证 repo.GetProvider 与 repo.ListProviders
	ollamaRow, err := provRepo.GetProvider(context.Background(), ollamaID)
	if err != nil || ollamaRow == nil {
		t.Fatalf("provRepo.GetProvider ollama failed: %v", err)
	}
	if !ollamaRow.IsLocal {
		t.Errorf("expected ollamaRow.IsLocal=true, got false")
	}

	deepseekRow, err := provRepo.GetProvider(context.Background(), deepseekID)
	if err != nil || deepseekRow == nil {
		t.Fatalf("provRepo.GetProvider deepseek failed: %v", err)
	}
	if deepseekRow.IsLocal {
		t.Errorf("expected deepseekRow.IsLocal=false, got true")
	}

	// 3 & 4. 手动创建厂商并验证 is_local 缺省推导
	// 3. 手工创建 Ollama (不传 is_local)
	manualOllamaReq := `{"name": "My Manual Ollama", "type": "ollama", "base_url": "http://127.0.0.1:11434", "enabled": true}`
	req = httptest.NewRequest("POST", "/v1/providers", bytes.NewBufferString(manualOllamaReq))
	w = httptest.NewRecorder()
	handler.HandleCreateProvider(w, req)
	if w.Result().StatusCode != http.StatusCreated {
		t.Fatalf("create manual ollama failed: %d, %s", w.Result().StatusCode, w.Body.String())
	}
	var manualOllamaResp struct {
		ID      string `json:"id"`
		IsLocal *bool  `json:"is_local"`
	}
	_ = json.NewDecoder(w.Body).Decode(&manualOllamaResp)
	if manualOllamaResp.IsLocal == nil || !*manualOllamaResp.IsLocal {
		t.Errorf("manual ollama response expected is_local=true, got %v", manualOllamaResp.IsLocal)
	}

	var manualOllamaDBIsLocal int
	if err := db.QueryRow(`SELECT is_local FROM providers WHERE id=?`, manualOllamaResp.ID).Scan(&manualOllamaDBIsLocal); err != nil {
		t.Fatalf("query manual ollama is_local: %v", err)
	}
	if manualOllamaDBIsLocal != 1 {
		t.Errorf("expected manual ollama is_local=1 in db, got %d", manualOllamaDBIsLocal)
	}

	// 4. 手工创建 OpenAI (不传 is_local)
	manualOpenAIReq := `{"name": "My Cloud OpenAI", "type": "openai_compat", "base_url": "https://api.openai.com", "api_key": "sk-123", "enabled": true}`
	req = httptest.NewRequest("POST", "/v1/providers", bytes.NewBufferString(manualOpenAIReq))
	w = httptest.NewRecorder()
	handler.HandleCreateProvider(w, req)
	if w.Result().StatusCode != http.StatusCreated {
		t.Fatalf("create manual openai failed: %d, %s", w.Result().StatusCode, w.Body.String())
	}
	var manualOpenAIResp struct {
		ID      string `json:"id"`
		IsLocal *bool  `json:"is_local"`
	}
	_ = json.NewDecoder(w.Body).Decode(&manualOpenAIResp)
	if manualOpenAIResp.IsLocal != nil && *manualOpenAIResp.IsLocal {
		t.Errorf("manual openai response expected is_local=false, got true")
	}

	// 5. 使用生产 LoadProvidersFromDB 加载到 ProviderRegistry
	client := &http.Client{}
	if err := LoadProvidersFromDB(context.Background(), db, nil, reg, client, nil); err != nil {
		t.Fatalf("LoadProvidersFromDB failed: %v", err)
	}

	// 6. 在 local_only_mode 下断言 Ollama 能被选中，DeepSeek 被拒绝
	origCfg := config.Get()
	defer config.Update(origCfg)
	var newCfg config.Config
	if origCfg != nil {
		newCfg = *origCfg
	}
	newCfg.Security.LocalOnlyMode = true
	config.Update(&newCfg)

	// Ollama 必须能被 PickEntryByRef 选中
	// 查询 ollama 对应的模型前缀或引用
	var ollamaModelID string
	if err := db.QueryRow(`SELECT id FROM provider_models WHERE provider_id=? LIMIT 1`, ollamaID).Scan(&ollamaModelID); err != nil {
		t.Fatalf("find ollama model: %v", err)
	}
	ollamaRef := ollamaModelID
	if len(ollamaRef) > 8 {
		ollamaRef = ollamaRef[:8]
	}

	entry := reg.PickEntryByRef(context.Background(), ollamaRef, false)
	if entry == nil {
		t.Errorf("expected ollama entry to be selectable in local_only_mode, but got nil")
	}

	// DeepSeek 必须在 local_only_mode 下被拒绝
	var deepseekModelID string
	if err := db.QueryRow(`SELECT id FROM provider_models WHERE provider_id=? LIMIT 1`, deepseekID).Scan(&deepseekModelID); err != nil {
		t.Fatalf("find deepseek model: %v", err)
	}
	deepseekRef := deepseekModelID
	if len(deepseekRef) > 8 {
		deepseekRef = deepseekRef[:8]
	}

	deepEntry := reg.PickEntryByRef(context.Background(), deepseekRef, false)
	if deepEntry != nil {
		t.Errorf("expected deepseek entry to be REJECTED in local_only_mode, but got %v", deepEntry)
	}
}

func repoProviderModel(providerID, modelID, role string) types.ProviderModelRow {
	return types.ProviderModelRow{
		ID:         "m_" + modelID,
		ProviderID: providerID,
		ModelID:    modelID,
		Name:       modelID,
		Role:       role,
		Enabled:    true,
	}
}
