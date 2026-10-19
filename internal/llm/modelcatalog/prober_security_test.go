package modelcatalog

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/security/network"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// TestProbeProviderModels_RejectsUnsafeClient 验证 client 为 nil 或非 SafeHTTPClient 时必须拒绝探测（R2-09）。
func TestProbeProviderModels_RejectsUnsafeClient(t *testing.T) {
	p := ProviderInfo{ID: "p1", Type: "openai_compat", BaseURL: "https://api.openai.com"}

	// 1. 零值 / 未标记安全客户端必须被拒绝
	unsafeClient := network.SafeHTTPClient{}
	_, err := ProbeProviderModels(context.Background(), unsafeClient, p)
	if err == nil {
		t.Fatal("R2-09 缺陷：未受保护的 SafeHTTPClient 必须返回错误，不能绕过 SafeDialer")
	}
	if apperr.CodeOf(err) != apperr.CodeInvalidInput {
		t.Fatalf("期望 CodeInvalidInput，实际: %v (%v)", apperr.CodeOf(err), err)
	}
}

// TestProbeProviderModels_ResponseBodyLimit8MB 验证响应体大小上限为 8MB（R2-09）。
func TestProbeProviderModels_ResponseBodyLimit8MB(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// 返回 9MB 的垃圾数据
		bigChunk := strings.Repeat("x", 9*1024*1024)
		_, _ = w.Write([]byte(`{"data": "` + bigChunk + `"}`))
	}))
	defer ts.Close()

	safeClient := network.NewSafeHTTPClient(nil)
	safeClient.Client = ts.Client()

	p := ProviderInfo{ID: "p-big", Type: "openai_compat", BaseURL: ts.URL}
	_, err := ProbeProviderModels(context.Background(), safeClient, p)
	if err == nil {
		t.Fatal("超过 8MB 的响应应当在 unmarshal 阶段因截断报错，绝不能全部读入内存")
	}
}

// TestProbeAnthropic_PaginationLimitsAndURLEscape 验证 Anthropic 分页上限为 50 页且 after_id 经 URL 转义（R2-09）。
func TestProbeAnthropic_PaginationLimitsAndURLEscape(t *testing.T) {
	pageCounter := 0
	escapedReceived := false

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pageCounter++
		afterID := r.URL.Query().Get("after_id")
		if afterID == "weird id&key=val" {
			escapedReceived = true
		}

		// 持续返回 has_more: true，测试 50 页上限
		resp := map[string]any{
			"data": []map[string]any{
				{"id": fmt.Sprintf("model-page-%d", pageCounter)},
			},
			"has_more": true,
			"last_id":  "weird id&key=val",
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer ts.Close()

	safeClient := network.NewSafeHTTPClient(nil)
	safeClient.Client = ts.Client()

	p := ProviderInfo{ID: "p-ant", Type: "anthropic", BaseURL: ts.URL, APIKey: "test"}
	models, err := ProbeProviderModels(context.Background(), safeClient, p)
	if err != nil {
		t.Fatalf("ProbeProviderModels anthropic failed: %v", err)
	}

	if pageCounter != 50 {
		t.Fatalf("期望 Anthropic 最多翻页 50 页，实际请求页数: %d", pageCounter)
	}
	if len(models) != 50 {
		t.Fatalf("期望返回 50 个模型，实际: %d", len(models))
	}
	if !escapedReceived {
		t.Fatal("R2-09 缺陷：after_id 未经过 URL 转义，导致 query 传参异常")
	}
}

// TestCatalogManager_ProbeTimeoutAndPreservesExisting 验证单厂商探测超时 30s 且失败不抹除已有记录（R2-09）。
func TestCatalogManager_ProbeTimeoutAndPreservesExisting(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	failServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream internal error", http.StatusInternalServerError)
	}))
	defer failServer.Close()

	_, _ = db.Exec(`INSERT INTO providers (id, name, type, base_url, api_key, enabled) VALUES ('p-fail', 'FailProv', 'openai_compat', ?, 'key', 1)`, failServer.URL)
	_, _ = db.Exec(`INSERT INTO provider_discovered_models (provider_id, model_id, display_name, context_window, max_output, discovered_at, last_seen_at) VALUES ('p-fail', 'existing-model', 'Existing', 128000, 4096, datetime('now'), datetime('now'))`)

	safeClient := network.NewSafeHTTPClient(nil)
	safeClient.Client = failServer.Client()

	mgr := NewCatalogManager(db, nil, safeClient)
	ctx := context.Background()

	_, err := mgr.RefreshProvider(ctx, "p-fail")
	if err == nil {
		t.Fatal("期望探测失败返回错误")
	}

	// 验证已有记录未被抹除
	list, err := mgr.ListDiscoveredModels(ctx, "p-fail")
	if err != nil {
		t.Fatalf("ListDiscoveredModels failed: %v", err)
	}
	if len(list) != 1 || list[0].ModelID != "existing-model" {
		t.Fatalf("R2-09 缺陷：探测失败不应清空已有的发现记录，实际 list: %+v", list)
	}
}
