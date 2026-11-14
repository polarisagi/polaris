package marketplace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/extension/pluginspec"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/network"
	"github.com/polarisagi/polaris/pkg/types"
)

type testDocRepo struct {
	protocol.ExtensionRepository
	mu        sync.RWMutex
	instances []types.ExtInstanceRow
	docIndex  map[string]types.ExtDocIndexRow
	tasks     map[string]types.ExtDocTranslationTaskRow
	markets   map[string]protocol.Marketplace
}

func newTestDocRepo() *testDocRepo {
	return &testDocRepo{
		docIndex: make(map[string]types.ExtDocIndexRow),
		tasks:    make(map[string]types.ExtDocTranslationTaskRow),
		markets:  make(map[string]protocol.Marketplace),
	}
}

func (r *testDocRepo) ListInstances(ctx context.Context) ([]types.ExtInstanceRow, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.instances, nil
}

func (r *testDocRepo) GetCatalogEntry(ctx context.Context, id string) (*types.ExtCatalogRow, error) {
	return nil, nil
}

func (r *testDocRepo) GetDocIndexEntry(ctx context.Context, catalogID string) (*types.ExtDocIndexRow, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if entry, ok := r.docIndex[catalogID]; ok {
		return &entry, nil
	}
	return nil, nil
}

func (r *testDocRepo) UpsertDocTranslationTask(ctx context.Context, task types.ExtDocTranslationTaskRow) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tasks[task.TaskID] = task
	return nil
}

func (r *testDocRepo) GetDocTranslationTask(ctx context.Context, taskID string) (*types.ExtDocTranslationTaskRow, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if task, ok := r.tasks[taskID]; ok {
		return &task, nil
	}
	return nil, nil
}

func (r *testDocRepo) FindDocTranslationTask(ctx context.Context, principalID, catalogID, sourceSHA256, locale, generatorVersion string) (*types.ExtDocTranslationTaskRow, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, t := range r.tasks {
		if t.PrincipalID == principalID && t.CatalogID == catalogID && t.SourceSHA256 == sourceSHA256 && t.Locale == locale && t.GeneratorVersion == generatorVersion {
			return &t, nil
		}
	}
	return nil, nil
}

func (r *testDocRepo) UpdateDocTranslationTaskStatus(ctx context.Context, taskID, status, errorCode string, retryAfter int, resultCachePath string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if t, ok := r.tasks[taskID]; ok {
		t.Status = status
		t.ErrorCode = errorCode
		t.RetryAfter = retryAfter
		t.ResultCachePath = resultCachePath
		r.tasks[taskID] = t
	}
	return nil
}

func (r *testDocRepo) ListPendingDocTranslationTasks(ctx context.Context, limit int) ([]types.ExtDocTranslationTaskRow, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var list []types.ExtDocTranslationTaskRow
	for _, t := range r.tasks {
		if t.Status == "queued" || t.Status == "translating" {
			list = append(list, t)
		}
	}
	return list, nil
}

func (r *testDocRepo) GetMarketplace(ctx context.Context, id string) (*protocol.Marketplace, error) {
	if mp, ok := r.markets[id]; ok {
		return &mp, nil
	}
	return nil, nil
}

type fakeLLMProvider struct {
	response string
	err      error
}

func (f *fakeLLMProvider) ID() string      { return "fake-provider" }
func (f *fakeLLMProvider) ModelID() string { return "fake-model" }
func (f *fakeLLMProvider) Capabilities() types.ProviderCapabilities {
	return types.ProviderCapabilities{}
}
func (f *fakeLLMProvider) Tokenizer() protocol.TokenizerAdapter { return nil }
func (f *fakeLLMProvider) Infer(ctx context.Context, msgs []types.Message, opts ...types.InferOption) (*types.ProviderResponse, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &types.ProviderResponse{
		Content: f.response,
	}, nil
}
func (f *fakeLLMProvider) StreamInfer(ctx context.Context, msgs []types.Message, opts ...types.InferOption) (<-chan types.StreamEvent, error) {
	ch := make(chan types.StreamEvent)
	close(ch)
	return ch, nil
}

func TestDocLocator_ResolutionOrder(t *testing.T) {
	tmpDir := t.TempDir()
	dataDir := filepath.Join(tmpDir, "data")
	instDir := filepath.Join(dataDir, "extensions", "installed", "custom", "my-plugin")
	if err := os.MkdirAll(instDir, 0o755); err != nil {
		t.Fatal(err)
	}

	enMarkdown := "# My Plugin\n\nThis is an English readme."
	if err := os.WriteFile(filepath.Join(instDir, "README.md"), []byte(enMarkdown), 0o644); err != nil {
		t.Fatal(err)
	}
	enSum := sha256.Sum256([]byte(enMarkdown))
	enHash := hex.EncodeToString(enSum[:])

	repo := newTestDocRepo()
	repo.instances = []types.ExtInstanceRow{
		{
			ID:          "ext_1",
			CatalogID:   "custom/my-plugin",
			Origin:      "user",
			InstallPath: instDir,
		},
	}

	locator := NewDocLocator(repo, network.NewSafeHTTPClient(nil), dataDir, "", "")
	ctx := context.Background()

	// 1. 英文请求返回英文
	respEn, err := locator.ResolveDocumentation(ctx, "custom/my-plugin", "en", "user1")
	if err != nil {
		t.Fatalf("ResolveDocumentation en failed: %v", err)
	}
	if respEn.Status != "ready" || respEn.Markdown != enMarkdown || *respEn.ResolvedLocale != "en" {
		t.Fatalf("unexpected en response: %+v", respEn)
	}

	// 2. 中文请求，当前无中文翻译，返回英文兜底
	respZhFallback, err := locator.ResolveDocumentation(ctx, "custom/my-plugin", "zh-CN", "user1")
	if err != nil {
		t.Fatalf("ResolveDocumentation fallback failed: %v", err)
	}
	if respZhFallback.Status != "ready" || *respZhFallback.ResolvedLocale != "en" || *respZhFallback.FallbackReason != "untranslated" {
		t.Fatalf("expected fallback untranslated, got: %+v", respZhFallback)
	}

	// 3. 上游自带 README_zh.md 时优先返回
	zhMarkdown := "# 我的插件\n\n这是中文说明。"
	if err := os.WriteFile(filepath.Join(instDir, "README_zh.md"), []byte(zhMarkdown), 0o644); err != nil {
		t.Fatal(err)
	}
	respZhUpstream, err := locator.ResolveDocumentation(ctx, "custom/my-plugin", "zh-CN", "user1")
	if err != nil {
		t.Fatalf("ResolveDocumentation upstream zh failed: %v", err)
	}
	if respZhUpstream.Status != "ready" || *respZhUpstream.ResolvedLocale != "zh-CN" || respZhUpstream.Markdown != zhMarkdown {
		t.Fatalf("expected upstream zh, got: %+v", respZhUpstream)
	}
	if respZhUpstream.Translation == nil || respZhUpstream.Translation.Origin != "bundled" || respZhUpstream.Translation.Review != "unreviewed" || !respZhUpstream.Stale {
		t.Fatalf("translation info mismatch (expected bundled/unreviewed/stale): %+v stale=%v", respZhUpstream.Translation, respZhUpstream.Stale)
	}

	// 4. 缺失源文档时返回 missing
	respMissing, err := locator.ResolveDocumentation(ctx, "custom/nonexistent", "zh-CN", "user1")
	if err != nil {
		t.Fatalf("ResolveDocumentation missing failed: %v", err)
	}
	if respMissing.Status != "missing" || respMissing.Source != nil {
		t.Fatalf("expected missing, got: %+v", respMissing)
	}

	// 5. 校验 enHash 变量未被未使用检查阻断
	_ = enHash
}

func TestDocTranslationService(t *testing.T) {
	tmpDir := t.TempDir()
	dataDir := filepath.Join(tmpDir, "data")
	instDir := filepath.Join(dataDir, "extensions", "installed", "custom", "test-tool")
	if err := os.MkdirAll(instDir, 0o755); err != nil {
		t.Fatal(err)
	}

	enContent := "# Test Tool\n\nDo some calculation."
	if err := os.WriteFile(filepath.Join(instDir, "README.md"), []byte(enContent), 0o644); err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256([]byte(enContent))
	enHash := hex.EncodeToString(h[:])

	repo := newTestDocRepo()
	repo.instances = []types.ExtInstanceRow{
		{
			ID:          "inst_tool",
			CatalogID:   "custom/test-tool",
			Origin:      "user",
			InstallPath: instDir,
		},
		{
			ID:          "inst_builtin",
			CatalogID:   "polarisagi/polaris-plugins-official/weather",
			Origin:      "builtin",
			InstallPath: filepath.Join(dataDir, "builtin"),
		},
	}

	locator := NewDocLocator(repo, network.NewSafeHTTPClient(nil), dataDir, "", "")
	provider := &fakeLLMProvider{
		response: "# 测试工具\n\n执行一些计算。",
	}

	service := NewDocTranslationService(repo, locator, provider, dataDir)
	ctx := context.Background()
	service.Start(ctx)
	defer service.Stop()

	// 1. 内置来源 POST 触发翻译必须被拒绝（403 builtin_translation_managed）
	_, err := service.EnqueueTranslation(ctx, "user1", "polarisagi/polaris-plugins-official/weather", "zh-CN", "any_hash")
	if err == nil {
		t.Fatal("expected error for builtin translation, got nil")
	}
	if !strings.Contains(err.Error(), "builtin_translation_managed") {
		t.Fatalf("expected builtin_translation_managed, got %v", err)
	}

	// 2. 源哈希不一致返回 409 source_changed
	_, err = service.EnqueueTranslation(ctx, "user1", "custom/test-tool", "zh-CN", "mismatched_hash")
	if err == nil || !strings.Contains(err.Error(), "source_changed") {
		t.Fatalf("expected source_changed, got %v", err)
	}

	// 3. 正确提交入队
	task, err := service.EnqueueTranslation(ctx, "user1", "custom/test-tool", "zh-CN", enHash)
	if err != nil {
		t.Fatalf("EnqueueTranslation failed: %v", err)
	}
	if task.Status != "queued" {
		t.Fatalf("expected queued status, got: %s", task.Status)
	}

	// 等待后台 worker 完成
	for i := 0; i < 20; i++ {
		time.Sleep(50 * time.Millisecond)
		tState, _ := service.GetTask(ctx, "user1", task.TaskID)
		if tState != nil && tState.Status == "ready" {
			break
		}
	}

	taskReady, err := service.GetTask(ctx, "user1", task.TaskID)
	if err != nil || taskReady == nil || taskReady.Status != "ready" {
		t.Fatalf("expected task ready, got: %+v, err=%v", taskReady, err)
	}

	// 4. 鉴权隔离：非 owner 查任务返回 404
	_, err = service.GetTask(ctx, "other_user", task.TaskID)
	if err == nil {
		t.Fatal("expected 404 for different user, got nil")
	}

	// 5. 验证之后通过 locator.ResolveDocumentation 能查到缓存翻译
	respResolved, err := locator.ResolveDocumentation(ctx, "custom/test-tool", "zh-CN", "user1")
	if err != nil {
		t.Fatalf("ResolveDocumentation after translation failed: %v", err)
	}
	if respResolved.Status != "ready" || respResolved.Markdown != "# 测试工具\n\n执行一些计算。" {
		t.Fatalf("expected cached translation, got: %+v", respResolved)
	}
}

func TestDocLocator_UninstalledDirectory(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "custom_market", "plugins/demo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("English"), 0o644); err != nil {
		t.Fatal(err)
	}
	me, _ := json.Marshal(pluginspec.MarketplaceEntry{Source: pluginspec.PluginSource{Type: pluginspec.SourceRelative, Path: "plugins/demo"}})
	payload, _ := json.Marshal(protocol.RegistryEntry{Entry: me})
	l := NewDocLocator(newTestDocRepo(), network.NewSafeHTTPClient(nil), root, root, "")
	source := l.readFromCatalogEntry(&types.ExtCatalogRow{ID: "custom/market/demo", MarketplaceID: "custom/market", Type: "plugin", Payload: string(payload)}, false)
	if source == nil || source.Content != "English" {
		t.Fatalf("expected uninstalled plugin docs to be located, got %+v", source)
	}
}

func TestDocLocator_DirectUserInstance(t *testing.T) {
	root := t.TempDir()
	instDir := filepath.Join(root, "installed")
	if err := os.MkdirAll(instDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(instDir, "README.md"), []byte("# Direct User"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := newTestDocRepo()
	r.instances = []types.ExtInstanceRow{{ID: "ext_direct", CatalogID: "", InstallPath: instDir, Origin: "user"}}
	l := NewDocLocator(r, network.NewSafeHTTPClient(nil), root, "", "")
	src, err := l.LocateSource(context.Background(), "ext_direct")
	if err != nil || src == nil || src.Content != "# Direct User" {
		t.Fatalf("expected ext_direct instance to locate source, got: %+v, err=%v", src, err)
	}
}

func TestDocLocator_SymlinkParentDefense(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	_ = os.WriteFile(filepath.Join(outside, "README.md"), []byte("outside content"), 0o644)
	link := filepath.Join(root, "package")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	l := NewDocLocator(nil, network.NewSafeHTTPClient(nil), root, "", "")
	result := l.readFromLocalDir(link, "custom/market/demo", false)
	if result != nil {
		t.Fatalf("expected symlinked parent escaping root to be rejected, got: %+v", result)
	}
}

type mutatingTruncatedProvider struct {
	path string
}

func (m *mutatingTruncatedProvider) ID() string      { return "mutating-provider" }
func (m *mutatingTruncatedProvider) ModelID() string { return "model" }
func (m *mutatingTruncatedProvider) Capabilities() types.ProviderCapabilities {
	return types.ProviderCapabilities{}
}
func (m *mutatingTruncatedProvider) Tokenizer() protocol.TokenizerAdapter { return nil }
func (m *mutatingTruncatedProvider) Infer(ctx context.Context, msgs []types.Message, opts ...types.InferOption) (*types.ProviderResponse, error) {
	_ = os.WriteFile(m.path, []byte("# New source while inference runs"), 0o644)
	return &types.ProviderResponse{Content: "# Truncated heading", FinishReason: "length"}, nil
}
func (m *mutatingTruncatedProvider) StreamInfer(ctx context.Context, msgs []types.Message, opts ...types.InferOption) (<-chan types.StreamEvent, error) {
	ch := make(chan types.StreamEvent)
	close(ch)
	return ch, nil
}

func TestDocTranslationService_StaleTruncatedOutputDefense(t *testing.T) {
	root := t.TempDir()
	instDir := filepath.Join(root, "installed")
	_ = os.MkdirAll(instDir, 0o755)
	path := filepath.Join(instDir, "README.md")
	_ = os.WriteFile(path, []byte("# English version one"), 0o644)
	r := newTestDocRepo()
	r.instances = []types.ExtInstanceRow{{CatalogID: "custom/market/demo", InstallPath: instDir, Origin: "user"}}
	l := NewDocLocator(r, network.NewSafeHTTPClient(nil), root, "", "")
	src, _ := l.LocateSource(context.Background(), "custom/market/demo")

	task := types.ExtDocTranslationTaskRow{
		TaskID:           "test_trunc",
		PrincipalID:      "u",
		CatalogID:        "custom/market/demo",
		SourceSHA256:     src.SHA256,
		Locale:           "zh-CN",
		GeneratorVersion: "v1",
		Status:           "queued",
	}
	_ = r.UpsertDocTranslationTask(context.Background(), task)

	mutatingProvider := &mutatingTruncatedProvider{path: path}
	s := NewDocTranslationService(r, l, mutatingProvider, root)
	s.processTask(context.Background(), task)

	actual, _ := r.GetDocTranslationTask(context.Background(), task.TaskID)
	if actual == nil || actual.Status != "failed" || actual.ErrorCode != "content_length_exceeded" {
		t.Fatalf("expected task to fail with content_length_exceeded, got: %+v", actual)
	}
}
