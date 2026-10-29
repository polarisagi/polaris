package plugin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/polarisagi/polaris/internal/extension/lifecycle"
	"github.com/polarisagi/polaris/internal/extension/pluginspec"
)

type stubAgentDefs []lifecycle.AgentDefinition

func (s stubAgentDefs) ListAgentDefinitions(context.Context) ([]lifecycle.AgentDefinition, []pluginspec.Diagnostic, error) {
	return s, []pluginspec.Diagnostic{{Severity: pluginspec.SeverityWarning, Component: "agent", Rule: pluginspec.RuleAgentNotApplied}}, nil
}

func TestHandleListAgents(t *testing.T) {
	h := &PluginHandler{}
	w := httptest.NewRecorder()
	h.HandleListAgents(w, httptest.NewRequest("GET", "/v1/agents", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured: %d", w.Code)
	}
	h.AgentDefs = stubAgentDefs{{Name: "review:security", Source: "plugin:pl_r", NotApplied: []string{"hooks"}}}
	w = httptest.NewRecorder()
	h.HandleListAgents(w, httptest.NewRequest("GET", "/v1/agents", nil))
	var out struct {
		Agents      []lifecycle.AgentDefinition `json:"agents"`
		Diagnostics []pluginspec.Diagnostic     `json:"diagnostics"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &out) != nil ||
		len(out.Agents) != 1 || out.Agents[0].NotApplied[0] != "hooks" || len(out.Diagnostics) != 1 {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
}
