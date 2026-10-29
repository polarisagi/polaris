package plugin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/extension/lifecycle"
)

type stubDeps struct{}

func (stubDeps) Check(context.Context, string) ([]lifecycle.DependencyStatus, error) {
	return []lifecycle.DependencyStatus{{Name: "vault", State: lifecycle.DepMissing}}, nil
}
func (stubDeps) EnableBlocker(context.Context, string) error  { return nil }
func (stubDeps) EnforceAll(context.Context) ([]string, error) { return nil, nil }

func TestHandleListPluginDependencies(t *testing.T) {
	h := &PluginHandler{}
	req := httptest.NewRequest("GET", "/v1/plugins/pl_kit/dependencies", nil)
	req.SetPathValue("id", "pl_kit")
	w := httptest.NewRecorder()
	h.HandleListPluginDependencies(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured: %d", w.Code)
	}
	h.Dependencies = stubDeps{}
	w = httptest.NewRecorder()
	h.HandleListPluginDependencies(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"state":"missing"`) {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
}
