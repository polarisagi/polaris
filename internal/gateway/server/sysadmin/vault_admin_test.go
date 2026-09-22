package sysadmin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/gateway/authcontext"
	"github.com/polarisagi/polaris/internal/sysmgr/updater"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// TestHandleVaultRotateMasterKey_Unauthenticated 验证未认证请求被拒绝，不调用轮换闭包。
func TestHandleVaultRotateMasterKey_Unauthenticated(t *testing.T) {
	called := false
	h := &SysAdminHandler{RotateVaultMasterKey: func(context.Context) (int, error) {
		called = true
		return 0, nil
	}}
	req := httptest.NewRequest(http.MethodPost, "/v1/vault/rotate-master-key", nil)
	w := httptest.NewRecorder()
	h.HandleVaultRotateMasterKey(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("unauthenticated: expected 403, got %d (body: %s)", w.Code, w.Body.String())
	}
	if called {
		t.Errorf("unauthenticated request must not invoke RotateVaultMasterKey")
	}
}

// TestHandleVaultRotateMasterKey_NotConfigured 验证 RotateVaultMasterKey 为 nil 时 fail-closed。
func TestHandleVaultRotateMasterKey_NotConfigured(t *testing.T) {
	h := &SysAdminHandler{}
	req := httptest.NewRequest(http.MethodPost, "/v1/vault/rotate-master-key", nil)
	ctx := authcontext.WithAuthContext(req.Context(), &authcontext.AuthContext{UserID: "local", Authenticated: true})
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	h.HandleVaultRotateMasterKey(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("nil rotator: expected 500, got %d (body: %s)", w.Code, w.Body.String())
	}
}

// TestHandleVaultRotateMasterKey_RotateError 验证闭包返回错误时响应 500，不触发重启。
func TestHandleVaultRotateMasterKey_RotateError(t *testing.T) {
	h := &SysAdminHandler{RotateVaultMasterKey: func(context.Context) (int, error) {
		return 0, apperr.New(apperr.CodeInternal, "boom")
	}}
	req := httptest.NewRequest(http.MethodPost, "/v1/vault/rotate-master-key", nil)
	ctx := authcontext.WithAuthContext(req.Context(), &authcontext.AuthContext{UserID: "local", Authenticated: true})
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	h.HandleVaultRotateMasterKey(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("rotate error: expected 500, got %d (body: %s)", w.Code, w.Body.String())
	}
}

// TestHandleVaultRotateMasterKey_SuccessTriggersRestart 验证成功轮换后响应 200 并异步触发 Updater.Restart()。
func TestHandleVaultRotateMasterKey_SuccessTriggersRestart(t *testing.T) {
	h := &SysAdminHandler{RotateVaultMasterKey: func(context.Context) (int, error) {
		return 3, nil
	}}
	restarted := make(chan struct{}, 1)
	m := updater.New("dev", "", "", nil)
	m.SetRestartFn(func() { restarted <- struct{}{} })
	h.Updater = m

	req := httptest.NewRequest(http.MethodPost, "/v1/vault/rotate-master-key", nil)
	ctx := authcontext.WithAuthContext(req.Context(), &authcontext.AuthContext{UserID: "local", ClientType: authcontext.ClientTypeLocal, Authenticated: true})
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	h.HandleVaultRotateMasterKey(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}
	var resp struct {
		Status           string `json:"status"`
		ProvidersRotated int    `json:"providers_rotated"`
		Restarting       bool   `json:"restarting"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if resp.Status != "rotated" || resp.ProvidersRotated != 3 || !resp.Restarting {
		t.Errorf("unexpected response: %+v", resp)
	}

	select {
	case <-restarted:
	case <-time.After(2 * time.Second):
		t.Fatal("restart was not triggered")
	}
}
