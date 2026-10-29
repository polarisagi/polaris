package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/polarisagi/polaris/internal/extension/lifecycle"
	"github.com/polarisagi/polaris/internal/extension/pluginspec"
	"github.com/polarisagi/polaris/pkg/apperr"
)

type fakePluginConfig struct {
	saved   []lifecycle.ConfigUpdate
	saveErr error
}

func (f *fakePluginConfig) GetSchema(_ context.Context, id string) ([]lifecycle.ConfigScope, error) {
	if id != "pl_x" {
		return nil, apperr.New(apperr.CodeNotFound, "plugin not found")
	}
	return []lifecycle.ConfigScope{{Options: []pluginspec.UserConfigOption{{Key: "token", Type: "string", Sensitive: true}}}}, nil
}

func (f *fakePluginConfig) ListValues(context.Context, string) ([]lifecycle.ConfigValue, error) {
	return []lifecycle.ConfigValue{{Key: "token", Sensitive: true, IsSet: true}}, nil
}

func (f *fakePluginConfig) SaveValues(_ context.Context, _ string, u []lifecycle.ConfigUpdate) error {
	f.saved = u
	return f.saveErr
}

func TestHandleGetPluginConfig(t *testing.T) {
	h := getDummyServerWithInstallMgr(t)
	h.PluginConfig = &fakePluginConfig{}

	req := httptest.NewRequest("GET", "/v1/plugins/pl_x/config", nil)
	req.SetPathValue("id", "pl_x")
	w := httptest.NewRecorder()
	h.HandleGetPluginConfig(w, req)
	if w.Code != http.StatusOK || bytes.Contains(w.Body.Bytes(), []byte(`"value"`)) {
		t.Fatalf("status=%d body=%s (sensitive value must not be serialized)", w.Code, w.Body.String())
	}

	req = httptest.NewRequest("GET", "/v1/plugins/missing/config", nil)
	req.SetPathValue("id", "missing")
	w = httptest.NewRecorder()
	h.HandleGetPluginConfig(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown plugin must be 404, got %d", w.Code)
	}
}

func TestHandleUpdatePluginConfig(t *testing.T) {
	h := getDummyServerWithInstallMgr(t)
	cfg := &fakePluginConfig{}
	h.PluginConfig = cfg

	body, _ := json.Marshal(map[string]any{"values": []map[string]any{{"key": "token", "value": "abc"}}})
	req := httptest.NewRequest("PUT", "/v1/plugins/pl_x/config", bytes.NewReader(body))
	req.SetPathValue("id", "pl_x")
	w := httptest.NewRecorder()
	h.HandleUpdatePluginConfig(w, req)
	if w.Code != http.StatusOK || len(cfg.saved) != 1 || cfg.saved[0].Key != "token" {
		t.Fatalf("status=%d saved=%+v body=%s", w.Code, cfg.saved, w.Body.String())
	}

	cfg.saveErr = apperr.New(apperr.CodeInvalidInput, "bad value")
	req = httptest.NewRequest("PUT", "/v1/plugins/pl_x/config", bytes.NewReader(body))
	req.SetPathValue("id", "pl_x")
	w = httptest.NewRecorder()
	h.HandleUpdatePluginConfig(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("validation error must map to 400, got %d", w.Code)
	}
}

func TestHandlePluginConfig_ServiceMissing(t *testing.T) {
	h := getDummyServerWithInstallMgr(t)
	w := httptest.NewRecorder()
	h.HandleGetPluginConfig(w, httptest.NewRequest("GET", "/v1/plugins/x/config", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 without config service, got %d", w.Code)
	}
}
