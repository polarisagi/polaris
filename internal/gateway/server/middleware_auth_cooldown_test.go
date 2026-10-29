package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestCheckAuth_LocalTokenBypassesIPCooldown ADR-0096 决策五 2026-09-25 追记：
// 重启后旧外壳持旧令牌轮询触发 127.0.0.1 冷却，持正确本地令牌的 CLI/新窗口不得被连坐；
// 远程 API Key 与错误令牌仍受冷却约束。
func TestCheckAuth_LocalTokenBypassesIPCooldown(t *testing.T) {
	const localTok = "local-token-abcdef"
	s := &Server{}
	s.SetLocalToken(localTok)
	am := NewAuthManager(context.Background())
	for range 20 {
		am.RecordFailure("127.0.0.1")
	}
	if !am.IsLocked("127.0.0.1") {
		t.Fatal("前置条件：127.0.0.1 应已进入冷却")
	}
	call := func(r *http.Request) int {
		w := httptest.NewRecorder()
		if _, ok := s.checkAuth(w, r, "127.0.0.1", "api-key", am); ok {
			return http.StatusOK
		}
		return w.Code
	}

	hdr := httptest.NewRequest("GET", "/v1/status", nil)
	hdr.Header.Set("Authorization", "Bearer "+localTok)
	if code := call(hdr); code != http.StatusOK {
		t.Errorf("正确本地令牌（请求头）不应被 IP 冷却连坐，得到 %d", code)
	}

	ck := httptest.NewRequest("GET", "/v1/status", nil)
	ck.Host = "127.0.0.1:28888"
	ck.AddCookie(&http.Cookie{Name: localTokenCookie, Value: localTok})
	if code := call(ck); code != http.StatusOK {
		t.Errorf("正确本地令牌（Cookie）不应被 IP 冷却连坐，得到 %d", code)
	}

	api := httptest.NewRequest("GET", "/v1/status", nil)
	api.Header.Set("Authorization", "Bearer api-key")
	if code := call(api); code != http.StatusTooManyRequests {
		t.Errorf("远程 API Key 可能是弱口令，必须仍受冷却约束，得到 %d", code)
	}

	bad := httptest.NewRequest("GET", "/v1/status", nil)
	bad.Header.Set("Authorization", "Bearer stale-token")
	if code := call(bad); code != http.StatusTooManyRequests {
		t.Errorf("错误令牌必须仍被冷却拒绝，得到 %d", code)
	}
}

// TestCheckAuth_LocalTokenDoesNotClearCooldown 冷却期内的本地令牌成功不得复位冷却，
// 否则本机正常流量会顺手解除对 API Key 猜测的限制。
func TestCheckAuth_LocalTokenDoesNotClearCooldown(t *testing.T) {
	s := &Server{}
	s.SetLocalToken("local-token-abcdef")
	am := NewAuthManager(context.Background())
	for range 20 {
		am.RecordFailure("127.0.0.1")
	}
	r := httptest.NewRequest("GET", "/v1/status", nil)
	r.Header.Set("Authorization", "Bearer local-token-abcdef")
	if _, ok := s.checkAuth(httptest.NewRecorder(), r, "127.0.0.1", "api-key", am); !ok {
		t.Fatal("本地令牌应放行")
	}
	if !am.IsLocked("127.0.0.1") {
		t.Fatal("本地令牌成功不得解除该 IP 对 API Key 的冷却")
	}
}
