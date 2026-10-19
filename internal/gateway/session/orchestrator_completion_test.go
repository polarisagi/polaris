package session

import (
	"context"
	"testing"

	"github.com/polarisagi/polaris/pkg/types"
)

// TestRunTurn_Headless_LoopCompletionByChannel 验证 R4-04：
// webhook 渠道默认 auto，cron/workflow 渠道默认 explicit。
func TestRunTurn_Headless_LoopCompletionByChannel(t *testing.T) {
	persistence := newFakePersistence()
	pool := &fakeAgentPool{headlessResult: &types.AgentResult{Output: "ok"}}
	orc := newTestOrchestrator(t, persistence, &fakeHooks{}, &fakeSlash{}, &fakeCompression{}, pool)

	// 1. Webhook 渠道（人类对话），默认 auto
	_, err := orc.RunTurn(context.Background(), Request{
		SessionID: "webhook-s1",
		Input:     "hello from telegram",
		Channel:   "webhook",
		Headless:  true,
	}, NewBufferSink())
	if err != nil {
		t.Fatalf("RunTurn webhook error: %v", err)
	}
	var opt types.HeadlessOptions
	for _, o := range pool.lastHeadlessOpts {
		o(&opt)
	}
	if opt.LoopCompletion != "auto" {
		t.Fatalf("webhook channel expected LoopCompletion=auto, got %q", opt.LoopCompletion)
	}

	// 2. Cron 渠道（无人值守后台自动化），默认 explicit
	_, err = orc.RunTurn(context.Background(), Request{
		SessionID: "cron-s1",
		Input:     "run nightly report",
		Channel:   "cron",
		Headless:  true,
	}, NewBufferSink())
	if err != nil {
		t.Fatalf("RunTurn cron error: %v", err)
	}
	opt = types.HeadlessOptions{}
	for _, o := range pool.lastHeadlessOpts {
		o(&opt)
	}
	if opt.LoopCompletion != "explicit" {
		t.Fatalf("cron channel expected LoopCompletion=explicit, got %q", opt.LoopCompletion)
	}

	// 3. 显式覆盖 LoopCompletion 优先
	_, err = orc.RunTurn(context.Background(), Request{
		SessionID:      "cron-s2",
		Input:          "custom override",
		Channel:        "cron",
		Headless:       true,
		LoopCompletion: "auto",
	}, NewBufferSink())
	if err != nil {
		t.Fatalf("RunTurn override error: %v", err)
	}
	opt = types.HeadlessOptions{}
	for _, o := range pool.lastHeadlessOpts {
		o(&opt)
	}
	if opt.LoopCompletion != "auto" {
		t.Fatalf("explicit override expected LoopCompletion=auto, got %q", opt.LoopCompletion)
	}
}
