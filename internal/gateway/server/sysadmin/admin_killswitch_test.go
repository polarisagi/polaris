package sysadmin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/polarisagi/polaris/internal/gateway/authcontext"
	"github.com/polarisagi/polaris/internal/security"
)

// newTestKillSwitch 创建一个使用临时目录的 KillSwitch 实例，供测试复用。
func newTestKillSwitch(t *testing.T) *security.KillSwitch {
	t.Helper()
	return security.NewKillSwitch(t.TempDir(), nil)
}

// TestHandleUnseal_AnonymousRejected 验证匿名用户调用 /_admin/unseal 被拒绝（B1 验收）。
func TestHandleUnseal_AnonymousRejected(t *testing.T) {
	ks := newTestKillSwitch(t)
	h := &SysAdminHandler{KillSwitch: ks}

	body, _ := json.Marshal(KillSwitchReq{Reason: "test"})
	req := httptest.NewRequest(http.MethodPost, "/_admin/unseal", bytes.NewReader(body))
	// 不注入 authcontext → FromContext 返回 UserID="anonymous"
	w := httptest.NewRecorder()
	h.HandleUnseal(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("anonymous: expected 403, got %d (body: %s)", w.Code, w.Body.String())
	}
}

// TestHandleUnseal_NonAdminRejected 验证非 admin 用户被拒绝，loopback 豁免不生效（B1 验收）。
func TestHandleUnseal_NonAdminRejected(t *testing.T) {
	ks := newTestKillSwitch(t)
	h := &SysAdminHandler{KillSwitch: ks}

	body, _ := json.Marshal(KillSwitchReq{Reason: "test"})
	req := httptest.NewRequest(http.MethodPost, "/_admin/unseal", bytes.NewReader(body))
	ctx := authcontext.WithAuthContext(req.Context(), &authcontext.AuthContext{UserID: "alice"})
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	h.HandleUnseal(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("non-admin: expected 403, got %d (body: %s)", w.Code, w.Body.String())
	}
}

// TestHandleUnseal_AdminSucceeds 验证 admin 用户可以成功 unseal（B1 验收）。
func TestHandleUnseal_AdminSucceeds(t *testing.T) {
	ks := newTestKillSwitch(t)
	// 先触发 FullStop，再测 unseal
	ks.ManualFullStop("admin", "test seal")

	h := &SysAdminHandler{KillSwitch: ks}

	body, _ := json.Marshal(KillSwitchReq{Reason: "manual recovery"})
	req := httptest.NewRequest(http.MethodPost, "/_admin/unseal", bytes.NewReader(body))
	ctx := authcontext.WithAuthContext(req.Context(), &authcontext.AuthContext{UserID: "admin", Authenticated: true})
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	h.HandleUnseal(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("admin: expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if resp["status"] != "unsealed" {
		t.Errorf("expected status=unsealed, got %q", resp["status"])
	}
}

// TestHandleUnseal_EmptyReasonRejected 验证空 reason 被拒绝。
func TestHandleUnseal_EmptyReasonRejected(t *testing.T) {
	ks := newTestKillSwitch(t)
	h := &SysAdminHandler{KillSwitch: ks}

	body, _ := json.Marshal(KillSwitchReq{Reason: ""})
	req := httptest.NewRequest(http.MethodPost, "/_admin/unseal", bytes.NewReader(body))
	ctx := authcontext.WithAuthContext(req.Context(), &authcontext.AuthContext{UserID: "admin", Authenticated: true})
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	h.HandleUnseal(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("empty reason: expected 400, got %d", w.Code)
	}
}

// TestHandleKill_AnonymousRejected_WebUI 验证未认证的 webui 匿名请求调用 /_admin/killswitch 被拒绝 (NEW-001 验收)。
func TestHandleKill_AnonymousRejected_WebUI(t *testing.T) {
	ks := newTestKillSwitch(t)
	h := &SysAdminHandler{KillSwitch: ks}

	body, _ := json.Marshal(KillSwitchReq{Reason: "emergency stop"})
	req := httptest.NewRequest(http.MethodPost, "/_admin/killswitch", bytes.NewReader(body))
	ctx := authcontext.WithAuthContext(req.Context(), &authcontext.AuthContext{UserID: "anonymous", ClientType: authcontext.ClientTypeWebUI, Authenticated: false})
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	h.HandleKill(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("anonymous webui: expected 403, got %d (body: %s)", w.Code, w.Body.String())
	}
}

// TestHandleKill_AnonymousRejected_Unknown 验证未提供 context (默认 anonymous/unknown) 的请求被拒绝 (NEW-001 验收)。
func TestHandleKill_AnonymousRejected_Unknown(t *testing.T) {
	ks := newTestKillSwitch(t)
	h := &SysAdminHandler{KillSwitch: ks}

	body, _ := json.Marshal(KillSwitchReq{Reason: "emergency stop"})
	req := httptest.NewRequest(http.MethodPost, "/_admin/killswitch", bytes.NewReader(body))
	// 从 context 未注入 auth -> FromContext 返回 UserID: "anonymous", Authenticated: false
	w := httptest.NewRecorder()
	h.HandleKill(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("unauthenticated request: expected 403, got %d (body: %s)", w.Code, w.Body.String())
	}
}

// TestHandleKill_AdminSucceeds 验证已认证的 admin 请求可以成功触发 KillSwitch (NEW-001 验收)。
func TestHandleKill_AdminSucceeds(t *testing.T) {
	ks := newTestKillSwitch(t)
	h := &SysAdminHandler{KillSwitch: ks}

	body, _ := json.Marshal(KillSwitchReq{Reason: "manual emergency stop"})
	req := httptest.NewRequest(http.MethodPost, "/_admin/killswitch", bytes.NewReader(body))
	ctx := authcontext.WithAuthContext(req.Context(), &authcontext.AuthContext{UserID: "admin", ClientType: authcontext.ClientTypeAPI, Authenticated: true})
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	h.HandleKill(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("admin: expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if resp["status"] != "sealed" {
		t.Errorf("expected status=sealed, got %q", resp["status"])
	}
}
