package plugin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/polarisagi/polaris/internal/extension/marketplace"
	"github.com/polarisagi/polaris/internal/gateway/authcontext"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/network"
	"github.com/polarisagi/polaris/pkg/types"
)

type mockDocExtensionRepo struct {
	protocol.ExtensionRepository
	mu        sync.RWMutex
	catalogs  map[string]types.ExtCatalogRow
	instances []types.ExtInstanceRow
	docIndex  map[string]types.ExtDocIndexRow
	tasks     map[string]types.ExtDocTranslationTaskRow
	markets   map[string]protocol.Marketplace
}

func newMockDocExtensionRepo() *mockDocExtensionRepo {
	return &mockDocExtensionRepo{
		catalogs: make(map[string]types.ExtCatalogRow),
		docIndex: make(map[string]types.ExtDocIndexRow),
		tasks:    make(map[string]types.ExtDocTranslationTaskRow),
		markets:  make(map[string]protocol.Marketplace),
	}
}

func (m *mockDocExtensionRepo) GetCatalogEntry(ctx context.Context, id string) (*types.ExtCatalogRow, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if c, ok := m.catalogs[id]; ok {
		return &c, nil
	}
	return nil, nil
}

func (m *mockDocExtensionRepo) ListInstances(ctx context.Context) ([]types.ExtInstanceRow, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.instances, nil
}

func (m *mockDocExtensionRepo) GetMarketplace(ctx context.Context, id string) (*protocol.Marketplace, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if mp, ok := m.markets[id]; ok {
		return &mp, nil
	}
	return nil, nil
}

func (m *mockDocExtensionRepo) GetDocIndexEntry(ctx context.Context, catalogID string) (*types.ExtDocIndexRow, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if e, ok := m.docIndex[catalogID]; ok {
		return &e, nil
	}
	return nil, nil
}

func (m *mockDocExtensionRepo) UpsertDocTranslationTask(ctx context.Context, task types.ExtDocTranslationTaskRow) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tasks[task.TaskID] = task
	return nil
}

func (m *mockDocExtensionRepo) GetDocTranslationTask(ctx context.Context, taskID string) (*types.ExtDocTranslationTaskRow, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if t, ok := m.tasks[taskID]; ok {
		return &t, nil
	}
	return nil, nil
}

func (m *mockDocExtensionRepo) FindDocTranslationTask(ctx context.Context, principalID, catalogID, sourceSHA256, locale, generatorVersion string) (*types.ExtDocTranslationTaskRow, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, t := range m.tasks {
		if t.PrincipalID == principalID && t.CatalogID == catalogID && t.SourceSHA256 == sourceSHA256 && t.Locale == locale && t.GeneratorVersion == generatorVersion {
			return &t, nil
		}
	}
	return nil, nil
}

func (m *mockDocExtensionRepo) UpdateDocTranslationTaskStatus(ctx context.Context, taskID, status, errorCode string, retryAfter int, resultCachePath string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t, ok := m.tasks[taskID]; ok {
		t.Status = status
		t.ErrorCode = errorCode
		t.RetryAfter = retryAfter
		t.ResultCachePath = resultCachePath
		m.tasks[taskID] = t
	}
	return nil
}

func (m *mockDocExtensionRepo) ListPendingDocTranslationTasks(ctx context.Context, limit int) ([]types.ExtDocTranslationTaskRow, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var list []types.ExtDocTranslationTaskRow
	for _, t := range m.tasks {
		if t.Status == "queued" || t.Status == "translating" {
			list = append(list, t)
		}
	}
	return list, nil
}

type fakeDocProvider struct {
	response string
}

func (f *fakeDocProvider) ID() string      { return "fake-doc-provider" }
func (f *fakeDocProvider) ModelID() string { return "fake-model" }
func (f *fakeDocProvider) Capabilities() types.ProviderCapabilities {
	return types.ProviderCapabilities{}
}
func (f *fakeDocProvider) Tokenizer() protocol.TokenizerAdapter { return nil }
func (f *fakeDocProvider) Infer(ctx context.Context, msgs []types.Message, opts ...types.InferOption) (*types.ProviderResponse, error) {
	return &types.ProviderResponse{Content: f.response}, nil
}
func (f *fakeDocProvider) StreamInfer(ctx context.Context, msgs []types.Message, opts ...types.InferOption) (<-chan types.StreamEvent, error) {
	ch := make(chan types.StreamEvent)
	close(ch)
	return ch, nil
}

func setupTestPluginHandler(t *testing.T) (*PluginHandler, *mockDocExtensionRepo, string, string) {
	tmpDir := t.TempDir()
	dataDir := filepath.Join(tmpDir, "data")
	cacheDir := filepath.Join(dataDir, "cache")
	instDir := filepath.Join(dataDir, "extensions", "installed")

	repo := newMockDocExtensionRepo()

	// 准备一个自定义扩展已安装目录
	customPluginDir := filepath.Join(instDir, "user", "custom-calc")
	if err := os.MkdirAll(customPluginDir, 0o755); err != nil {
		t.Fatal(err)
	}
	readmeContent := "# Calculator Plugin\n\nPerform basic arithmetic."
	if err := os.WriteFile(filepath.Join(customPluginDir, "README.md"), []byte(readmeContent), 0o644); err != nil {
		t.Fatal(err)
	}

	h := sha256.Sum256([]byte(readmeContent))
	readmeHash := hex.EncodeToString(h[:])

	repo.instances = []types.ExtInstanceRow{
		{
			ID:          "inst_calc",
			CatalogID:   "user/custom-calc",
			Origin:      "user",
			InstallPath: customPluginDir,
		},
		{
			ID:          "inst_builtin_weather",
			CatalogID:   "polarisagi/polaris-plugins-official/weather-pro",
			Origin:      "builtin",
			InstallPath: filepath.Join(instDir, "builtin", "weather-pro"),
		},
	}
	repo.markets["polarisagi/polaris-plugins-official"] = protocol.Marketplace{
		ID:        "polarisagi/polaris-plugins-official",
		IsBuiltin: 1,
	}

	locator := marketplace.NewDocLocator(repo, network.NewSafeHTTPClient(nil), dataDir, cacheDir, instDir)
	provider := &fakeDocProvider{response: "# 计算器插件\n\n执行基础算术。"}
	transSvc := marketplace.NewDocTranslationService(repo, locator, provider, dataDir)
	docIndex := marketplace.NewDocIndexSync(repo, network.NewSafeHTTPClient(nil))

	hHandler := &PluginHandler{
		ExtRepo:         repo,
		DataDir:         dataDir,
		DocLocator:      locator,
		DocTranslations: transSvc,
		DocIndex:        docIndex,
	}

	return hHandler, repo, readmeHash, readmeContent
}

func TestHandleGetPluginDocumentation(t *testing.T) {
	h, repo, hash, content := setupTestPluginHandler(t)
	ctx := context.Background()
	_ = repo

	// 1. 缺失 catalog_id 参数 -> 400
	req := httptest.NewRequest(http.MethodGet, "/v1/plugins/documentation", nil)
	w := httptest.NewRecorder()
	h.HandleGetPluginDocumentation(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing catalog_id, got %d", w.Code)
	}

	// 2. 不存在的 catalog_id -> 404
	req = httptest.NewRequest(http.MethodGet, "/v1/plugins/documentation?catalog_id=missing/plugin", nil)
	w = httptest.NewRecorder()
	h.HandleGetPluginDocumentation(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for non-existent catalog_id, got %d", w.Code)
	}

	// 3. 英文请求 -> 200 返回原文
	req = httptest.NewRequest(http.MethodGet, "/v1/plugins/documentation?catalog_id=user/custom-calc&locale=en", nil)
	w = httptest.NewRecorder()
	h.HandleGetPluginDocumentation(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for en, got %d", w.Code)
	}
	var resp marketplace.DocumentationResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Status != "ready" || resp.Markdown != content || *resp.ResolvedLocale != "en" {
		t.Fatalf("unexpected response: %+v", resp)
	}
	if resp.Source == nil || resp.Source.SHA256 != hash {
		t.Fatalf("source info mismatch: %+v", resp.Source)
	}

	// 4. 中文请求未翻译 -> 200 返回原文，fallback_reason="untranslated"
	req = httptest.NewRequest(http.MethodGet, "/v1/plugins/documentation?catalog_id=user/custom-calc&locale=zh-CN", nil)
	w = httptest.NewRecorder()
	h.HandleGetPluginDocumentation(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var respZh marketplace.DocumentationResponse
	if err := json.Unmarshal(w.Body.Bytes(), &respZh); err != nil {
		t.Fatalf("failed to decode zh response: %v", err)
	}
	if respZh.Status != "ready" || *respZh.ResolvedLocale != "en" || *respZh.FallbackReason != "untranslated" {
		t.Fatalf("expected fallback response, got: %+v", respZh)
	}

	_ = ctx
}

func TestHandleCreatePluginTranslation_BuiltinRejected(t *testing.T) {
	h, _, hash, _ := setupTestPluginHandler(t)

	// 对内置扩展 POST 触发翻译必须强制返回 403 及稳定错误码 builtin_translation_managed
	body := map[string]string{
		"catalog_id":    "polarisagi/polaris-plugins-official/weather-pro",
		"locale":        "zh-CN",
		"source_sha256": hash,
	}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPost, "/v1/plugins/documentation/translations", bytes.NewReader(bodyBytes))
	w := httptest.NewRecorder()
	h.HandleCreatePluginTranslation(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden for builtin translation, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "builtin_translation_managed") {
		t.Fatalf("expected builtin_translation_managed error code, got: %s", w.Body.String())
	}
}

func TestHandleCreatePluginTranslation_ClientInjectionRejected(t *testing.T) {
	h, _, hash, _ := setupTestPluginHandler(t)

	// 严防客户端注入任何正文、模型密钥或 URL
	body := map[string]string{
		"catalog_id":    "user/custom-calc",
		"locale":        "zh-CN",
		"source_sha256": hash,
		"markdown":      "hacked content",
	}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPost, "/v1/plugins/documentation/translations", bytes.NewReader(bodyBytes))
	w := httptest.NewRecorder()
	h.HandleCreatePluginTranslation(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for client content injection, got %d", w.Code)
	}
}

func TestHandleCreatePluginTranslation_SourceChangedConflict(t *testing.T) {
	h, _, _, _ := setupTestPluginHandler(t)

	body := map[string]string{
		"catalog_id":    "user/custom-calc",
		"locale":        "zh-CN",
		"source_sha256": "stale_hash_12345",
	}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPost, "/v1/plugins/documentation/translations", bytes.NewReader(bodyBytes))
	w := httptest.NewRecorder()
	h.HandleCreatePluginTranslation(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 Conflict for mismatched source hash, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "source_changed") {
		t.Fatalf("expected source_changed error code, got: %s", w.Body.String())
	}
}

func TestHandleCreateAndGetPluginTranslationTask(t *testing.T) {
	h, _, hash, _ := setupTestPluginHandler(t)

	body := map[string]string{
		"catalog_id":    "user/custom-calc",
		"locale":        "zh-CN",
		"source_sha256": hash,
	}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPost, "/v1/plugins/documentation/translations", bytes.NewReader(bodyBytes))
	// 注入认证主体 alice
	req = req.WithContext(authcontext.WithAuthContext(req.Context(), &authcontext.AuthContext{UserID: "alice", Authenticated: true}))
	w := httptest.NewRecorder()
	h.HandleCreatePluginTranslation(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202 Accepted, got %d: %s", w.Code, w.Body.String())
	}

	var taskResp TranslationTaskResponse
	if err := json.Unmarshal(w.Body.Bytes(), &taskResp); err != nil {
		t.Fatalf("failed to decode task response: %v", err)
	}
	if taskResp.TaskID == "" || taskResp.Status != "queued" {
		t.Fatalf("unexpected task response: %+v", taskResp)
	}

	// 1. 同主体读取任务状态 -> 200
	reqGet := httptest.NewRequest(http.MethodGet, "/v1/plugins/documentation/translations?task_id="+taskResp.TaskID, nil)
	reqGet = reqGet.WithContext(authcontext.WithAuthContext(reqGet.Context(), &authcontext.AuthContext{UserID: "alice", Authenticated: true}))
	wGet := httptest.NewRecorder()
	h.HandleGetPluginTranslationTask(wGet, reqGet)

	if wGet.Code != http.StatusOK {
		t.Fatalf("expected 200 for task query, got %d", wGet.Code)
	}
	var taskStatus TranslationTaskResponse
	if err := json.Unmarshal(wGet.Body.Bytes(), &taskStatus); err != nil {
		t.Fatalf("failed to decode task status: %v", err)
	}
	if taskStatus.TaskID != taskResp.TaskID {
		t.Fatalf("task ID mismatch: got %s, want %s", taskStatus.TaskID, taskResp.TaskID)
	}

	// 2. 不同主体（bob）读取 alice 的任务 -> 404（主体隔离，不泄露）
	reqBob := httptest.NewRequest(http.MethodGet, "/v1/plugins/documentation/translations?task_id="+taskResp.TaskID, nil)
	reqBob = reqBob.WithContext(authcontext.WithAuthContext(reqBob.Context(), &authcontext.AuthContext{UserID: "bob", Authenticated: true}))
	wBob := httptest.NewRecorder()
	h.HandleGetPluginTranslationTask(wBob, reqBob)

	if wBob.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for different user query, got %d", wBob.Code)
	}
}
