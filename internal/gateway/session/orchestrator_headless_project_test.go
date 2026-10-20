package session

import (
	"context"
	"testing"

	"github.com/polarisagi/polaris/pkg/types"
)

// 自动化所属项目：无头回合把新会话归入 Request.ProjectID（此前恒为默认项目）。
func TestRunTurn_Headless_BindsAutomationProject(t *testing.T) {
	persistence := newFakePersistence()
	pool := &fakeAgentPool{headlessResult: &types.AgentResult{Output: "ok", LatencyMs: 1}}
	orc := newTestOrchestrator(t, persistence, &fakeHooks{}, &fakeSlash{}, &fakeCompression{}, pool)

	if _, err := orc.RunTurn(context.Background(), Request{
		SessionID: "auto-sess", ProjectID: "prj_a", Input: "go", Channel: "cron", Headless: true,
	}, NewBufferSink()); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if got := persistence.projects["auto-sess"]; got != "prj_a" {
		t.Errorf("session project = %q, want prj_a", got)
	}
}
