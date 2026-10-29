package main

import (
	"context"
	"fmt"
	"testing"

	"github.com/polarisagi/polaris/internal/action"
	"github.com/polarisagi/polaris/internal/learning/surprise"
	"github.com/polarisagi/polaris/internal/protocol/repo"
	"github.com/polarisagi/polaris/pkg/apperr"
)

type failingWarmReader struct{}

func (failingWarmReader) RecentToolRows(context.Context, int) ([]repo.TrajectoryRow, error) {
	return nil, apperr.New(apperr.CodeInternal, "db down")
}

func (failingWarmReader) RecentToolSequences(context.Context, int, int64) ([][]string, error) {
	return nil, apperr.New(apperr.CodeInternal, "db down")
}

func TestWarmStartPolicyEvolver_RestoresSuccessRateAndError(t *testing.T) {
	tr := newTestTrajectoryRepo(t)
	w := newStoreEventWriter(tr)
	// 4 次成功 + 1 次失败（带错误文本）；另一个工具与状态事件不应串入。
	for range 4 {
		w.WriteToolCallEvent("s1", "read_file", map[string]any{"path": "x"}, map[string]any{"ok": true}, true, 20)
	}
	w.WriteToolCallEvent("s1", "read_file", nil, map[string]any{"error": "ENOENT"}, false, 5)
	w.WriteToolCallEvent("s2", "bash", nil, nil, true, 1)
	w.WriteStateTransEvent("s1", "S_PLAN")

	pe := action.NewPolicyEvolver(0, 0)
	warmStartPolicyEvolver(context.Background(), tr, pe)

	if got := pe.SuccessRate("read_file"); got < 0.799 || got > 0.801 {
		t.Fatalf("read_file 成功率应为 0.8，got %f", got)
	}
	if pe.SuccessRate("bash") != 1 {
		t.Fatalf("bash 成功率应为 1，got %f", pe.SuccessRate("bash"))
	}
	if pe.SuccessRate("S_PLAN") != -1 {
		t.Fatal("状态事件不应进 PolicyEvolver")
	}
}

func TestWarmStartPolicyEvolver_OnlyLatestWindow(t *testing.T) {
	tr := newTestTrajectoryRepo(t)
	w := newStoreEventWriter(tr)
	pe := action.NewPolicyEvolver(5, 0)
	// 前 10 次失败、后 5 次成功：窗口 5 → 只应看到后 5 次。
	for range 10 {
		w.WriteToolCallEvent("s", "t", nil, nil, false, 1)
	}
	for range 5 {
		w.WriteToolCallEvent("s", "t", nil, nil, true, 1)
	}
	warmStartPolicyEvolver(context.Background(), tr, pe)
	if pe.SuccessRate("t") != 1 {
		t.Fatalf("只应恢复最近 window 条，got %f", pe.SuccessRate("t"))
	}
}

func TestToolOutcomesFromRows_ErrorOnlyOnFailure(t *testing.T) {
	yes, no := true, false
	rows := []repo.TrajectoryRow{
		{ToolName: "a", ToolOK: &yes, LatencyMs: 7, Payload: `{"result":{"error":"weird"}}`},
		{ToolName: "a", ToolOK: &no, LatencyMs: -1, Payload: `{"result":{"error":"boom"}}`},
		{ToolName: "a", ToolOK: nil, Payload: `not json`},
		{ToolName: "", Payload: `{}`},
	}
	got := toolOutcomesFromRows(rows)
	if len(got) != 3 {
		t.Fatalf("无工具名的行应跳过: %+v", got)
	}
	if got[0].Error != "" || !got[0].Success || got[0].LatencyMs != 7 {
		t.Errorf("成功行不带 Error: %+v", got[0])
	}
	if got[1].Error != "boom" || got[1].Success || got[1].LatencyMs != 0 {
		t.Errorf("失败行还原错误文本、NULL 耗时按 0: %+v", got[1])
	}
	if got[2].Success || got[2].Error != "" {
		t.Errorf("NULL tool_ok 按失败、坏 payload 不 panic: %+v", got[2])
	}
}

func TestWarmStartSurprise_TransitionsFromTrajectory(t *testing.T) {
	tr := newTestTrajectoryRepo(t)
	w := newStoreEventWriter(tr)
	for i := range 3 {
		sid := fmt.Sprintf("s%d", i)
		for _, tool := range []string{"read", "edit", "bash"} {
			w.WriteToolCallEvent(sid, tool, nil, nil, true, 1)
		}
	}
	calc := surprise.NewSurpriseCalculator(nil)
	defer calc.Close()
	if calc.MarkovTransitions() != 0 {
		t.Fatal("测试前提：新计算器无转移")
	}
	warmStartSurprise(context.Background(), tr, calc)
	if got := calc.MarkovTransitions(); got != 6 { // 3 会话 × 2 次转移
		t.Fatalf("warm-start 后 TotalTransitions 应为 6，got %f", got)
	}
}

func TestWarmStart_ReadFailureDegradesQuietly(t *testing.T) {
	calc := surprise.NewSurpriseCalculator(nil)
	defer calc.Close()
	pe := action.NewPolicyEvolver(0, 0)
	warmStartSurprise(context.Background(), failingWarmReader{}, calc)
	warmStartPolicyEvolver(context.Background(), failingWarmReader{}, pe)
	if calc.MarkovTransitions() != 0 || pe.SuccessRate("x") != -1 {
		t.Fatal("读失败应保持冷启动")
	}
	// nil 参数不 panic。
	warmStartSurprise(context.Background(), nil, nil)
	warmStartPolicyEvolver(context.Background(), nil, nil)
}
