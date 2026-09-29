package graph

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/polarisagi/polaris/internal/protocol/repo"
)

// 任务执行状态画布：基于 Mermaid graph LR 的工具调用符号化渲染。
//
// 核心思想来自 TencentDB Agent Memory：
//   - 工具调用历史不做字节截断，而是提炼为结构化符号图注入上下文
//   - LLM 对 Mermaid 有强先验（训练数据中大量 GitHub README），解析效率高于等效 JSON
//   - 边关系（-->）原生表达执行流，JSON 平铺列表无法简洁表达
//
// 2026-09-29（ADR-0104 决策七）：画布不再是全进程共享的有状态单例（TaskMermaidCanvas +
// TrackToolCall/TrackToolResult，所有会话的工具调用混进同一张图），改为从会话的工具步骤列表
// 纯函数渲染；步骤取自 session_trajectory 的该会话工具行（MmdStepsFromTrajectory）。
//
// 典型输出（注入 anchor 后 LLM 可读）:
//
//	graph LR
//	  N1["read_file ✓ | 读取 config.go"] --> N2["bash ✗ | make build 失败"]
//	  N2["bash ✗ | make build 失败"] --> N3["edit_file ✓ | 修改 Makefile"]
//	  style N2 fill:#d64,color:#fff
//	  style N3 fill:#4a4,color:#fff
//
// 节点 token 估算: ~8 token/节点，20 节点画布约 160 token（TencentDB 实测 500 token 以内）。

const (
	mmdStatusSuccess = "✓"
	mmdStatusFailed  = "✗"

	mmdMaxLabelChars = 40 // 节点 summary 最大字符数，超出截断
	mmdMaxNodes      = 30 // 单 canvas 最大节点数，防止爆 token
)

// MmdStep 单个工具执行步骤记录。
type MmdStep struct {
	NodeID  string // 格式 "N{序号}"，如 "N1"、"N2"
	Tool    string // 工具名
	Status  string // mmdStatusSuccess | mmdStatusFailed
	Summary string // ≤40 字摘要
}

// MmdStepsFromTrajectory 把会话的工具轨迹行（须按 seq 升序）转成画布步骤。
// 只取最近 mmdMaxNodes 步：画布注入的是压缩摘要，越近的步骤对续写越有用。
// 摘要：失败取 payload.result.error，成功取 payload.result 的紧凑 JSON（键序确定），再截断。
// tool_ok 为 NULL（理论上不出现在工具行）按失败画，宁可提示失败也不虚报成功。
func MmdStepsFromTrajectory(rows []repo.TrajectoryRow) []MmdStep {
	steps := make([]MmdStep, 0, min(len(rows), mmdMaxNodes))
	for _, row := range rows {
		if row.ToolName == "" {
			continue
		}
		ok := row.ToolOK != nil && *row.ToolOK
		status := mmdStatusFailed
		if ok {
			status = mmdStatusSuccess
		}
		steps = append(steps, MmdStep{
			Tool:    row.ToolName,
			Status:  status,
			Summary: truncateLabel(trajectorySummary(row.Payload, ok)),
		})
	}
	if len(steps) > mmdMaxNodes {
		steps = steps[len(steps)-mmdMaxNodes:]
	}
	for i := range steps {
		steps[i].NodeID = fmt.Sprintf("N%d", i+1)
	}
	return steps
}

func trajectorySummary(payload string, ok bool) string {
	var p struct {
		Result map[string]any `json:"result"`
	}
	if json.Unmarshal([]byte(payload), &p) != nil || p.Result == nil {
		if ok {
			return ""
		}
		return "failed"
	}
	if !ok {
		if msg, _ := p.Result["error"].(string); msg != "" {
			return msg
		}
		return "failed"
	}
	b, err := json.Marshal(p.Result)
	if err != nil {
		return ""
	}
	return string(b)
}

// RenderMmdCanvas 把步骤列表渲染为注入 LLM 上下文的 Mermaid graph LR 文本。
// 相邻步骤顺序连边；空列表返回空字符串（调用方跳过注入）。纯函数，无状态。
func RenderMmdCanvas(steps []MmdStep) string {
	if len(steps) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("graph LR\n")

	for i := 1; i < len(steps); i++ {
		fmt.Fprintf(&sb, "  %s[\"%s\"] --> %s[\"%s\"]\n",
			steps[i-1].NodeID, mmdLabel(&steps[i-1]),
			steps[i].NodeID, mmdLabel(&steps[i]))
	}
	// 单步画布没有边，节点需单独成行。
	if len(steps) == 1 {
		fmt.Fprintf(&sb, "  %s[\"%s\"]\n", steps[0].NodeID, mmdLabel(&steps[0]))
	}

	// 节点样式：失败=红，成功=绿
	for _, s := range steps {
		switch s.Status {
		case mmdStatusFailed:
			fmt.Fprintf(&sb, "  style %s fill:#d64,color:#fff\n", s.NodeID)
		case mmdStatusSuccess:
			fmt.Fprintf(&sb, "  style %s fill:#4a4,color:#fff\n", s.NodeID)
		}
	}

	return sb.String()
}

// ─── 内部辅助 ─────────────────────────────────────────────────────────────────

// mmdLabel 生成 Mermaid 节点标签："tool status | summary"
func mmdLabel(s *MmdStep) string {
	label := s.Tool + " " + s.Status
	if s.Summary != "" {
		label += " | " + s.Summary
	}
	return escapeMmd(label)
}

// escapeMmd 转义 Mermaid 标签中的特殊字符。
// 双引号 → 单引号；方括号 → 圆括号；换行 → 空格。
func escapeMmd(s string) string {
	s = strings.ReplaceAll(s, `"`, `'`)
	s = strings.ReplaceAll(s, "[", "(")
	s = strings.ReplaceAll(s, "]", ")")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", "")
	return s
}

// truncateLabel 截断超过 mmdMaxLabelChars 的摘要，追加省略号。
func truncateLabel(s string) string {
	runes := []rune(s)
	if len(runes) <= mmdMaxLabelChars {
		return s
	}
	return string(runes[:mmdMaxLabelChars-1]) + "…"
}
