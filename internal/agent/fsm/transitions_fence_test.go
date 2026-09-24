package fsm

import (
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
)

// TestParsePlanOnSuccess_CodeFence 复现 2026-09-22 实测缺陷：模型把 DAG 包在
// ```json 代码围栏里返回（DeepSeek 实测，各家普遍如此），而 parsePlanOnSuccess
// 直接 json.Unmarshal 原文必然失败：
//
//	schemavalidate: plan_dag: invalid JSON: invalid character '`' ...
//
// 首轮没有缓存 DAGModel 可复用 → S_PLAN_FAILED → 重规划 → TriggerReplanExhausted
// → S_FAILED。即**每个需要规划的任务都必然失败**，Agent 实际只能产出对话文本。
func TestParsePlanOnSuccess_CodeFence(t *testing.T) {
	plan := `{"nodes":[{"id":"n1","action":"read_file","params":{"path":"a.txt"}}],"edges":[]}`

	for _, tc := range []struct {
		name    string
		content string
	}{
		{"裸 JSON", plan},
		{"```json 围栏", "```json\n" + plan + "\n```"},
		{"``` 围栏", "```\n" + plan + "\n```"},
		{"围栏外带解释文字", "这是计划：\n```json\n" + plan + "\n```\n以上。"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sCtx := &StateContext{}
			state, err := parsePlanOnSuccess(sCtx, protocol.StateContext{}, []byte(tc.content))
			if err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			if state != "S_PLAN_DONE" {
				t.Fatalf("state = %q, want S_PLAN_DONE", state)
			}
			if sCtx.DAGModel == nil || len(sCtx.DAGModel.Nodes) != 1 {
				t.Fatalf("DAGModel 未正确写入: %+v", sCtx.DAGModel)
			}
			if got := sCtx.DAGModel.Nodes[0].ToolName; got != "read_file" {
				t.Errorf("ToolName = %q, want read_file", got)
			}
		})
	}
}
