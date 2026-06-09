package cognition

import (
	"fmt"
	"strings"
	"sync"
)

// TaskMermaidCanvas 基于 Mermaid graph LR 的任务执行状态画布。
//
// 核心思想来自 TencentDB Agent Memory：
//   - 工具调用历史不做字节截断，而是提炼为结构化符号图注入上下文
//   - 每个节点携带 node_id，可用于 read_tool_ref drill-down 取回原始输出
//   - LLM 对 Mermaid 有强先验（训练数据中大量 GitHub README），解析效率高于等效 JSON
//   - 边关系（-->）原生表达执行流与分支，JSON 平铺列表无法简洁表达
//
// 典型输出（注入 anchor 后 LLM 可读）:
//
//	graph LR
//	  N1["read_file ✓ | 读取 config.go"] --> N2
//	  N2["bash ✗ | make build 失败"]
//	  N2 --> N3["edit_file ✓ | 修改 Makefile"]
//	  N3 --> N4["bash ✓ | build 成功"]
//	  style N2 fill:#f66,color:#fff
//	  style N4 fill:#6a6,color:#fff
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
	NodeID  string // 格式 "N{seq}"，如 "N1"、"N2"
	Tool    string // 工具名
	Status  string // mmdStatusSuccess | mmdStatusFailed | mmdStatusPending
	Summary string // ≤40 字摘要
	RefID   string // offloader 存储的 tool_use_id，供 read_tool_ref drill-down
}

// MmdEdge 节点间有向边（支持条件标注）。
type MmdEdge struct {
	From  string
	To    string
	Label string // 可选，如 "retry" / "fallback"
}

// TaskMermaidCanvas 线程安全的 Mermaid 画布。
type TaskMermaidCanvas struct {
	mu          sync.Mutex
	steps       []MmdStep
	edges       []MmdEdge
	pendingCalls map[string]string // tool_use_id → tool_name（等待结果）
	seq         int
}

// NewTaskMermaidCanvas 创建空画布。
func NewTaskMermaidCanvas() *TaskMermaidCanvas {
	return &TaskMermaidCanvas{
		pendingCalls: make(map[string]string),
	}
}

// TrackToolCall 记录工具调用开始，创建 pending 节点。
// toolUseID 对应 Anthropic tool_use_id 或 OpenAI tool_call_id。
func (c *TaskMermaidCanvas) TrackToolCall(toolUseID, toolName string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pendingCalls[toolUseID] = toolName
}

// TrackToolResult 将 pending 节点转为已完成节点，追加到画布。
// success=true → ✓ 绿色节点；success=false → ✗ 红色节点。
// summary 建议 ≤40 字，超出自动截断。
func (c *TaskMermaidCanvas) TrackToolResult(toolUseID string, success bool, summary string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	toolName, ok := c.pendingCalls[toolUseID]
	if !ok {
		toolName = "unknown"
	}
	delete(c.pendingCalls, toolUseID)

	if len(c.steps) >= mmdMaxNodes {
		return
	}

	c.seq++
	status := mmdStatusSuccess
	if !success {
		status = mmdStatusFailed
	}

	nodeID := fmt.Sprintf("N%d", c.seq)
	step := MmdStep{
		NodeID:  nodeID,
		Tool:    toolName,
		Status:  status,
		Summary: truncateLabel(summary),
		RefID:   toolUseID,
	}
	c.steps = append(c.steps, step)

	// 自动连接到前一个节点（顺序流）
	if len(c.steps) > 1 {
		prev := c.steps[len(c.steps)-2]
		c.edges = append(c.edges, MmdEdge{From: prev.NodeID, To: nodeID})
	}
}

// AddEdge 手动添加带标注的有向边，用于表达分支/重试/回退关系。
func (c *TaskMermaidCanvas) AddEdge(from, to, label string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.edges = append(c.edges, MmdEdge{From: from, To: to, Label: label})
}

// Render 生成注入 LLM 上下文的 Mermaid graph LR 文本。
// 空画布返回空字符串（调用方跳过注入）。
func (c *TaskMermaidCanvas) Render() string {
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(c.steps) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("graph LR\n")

	// 节点定义：已有前向边的节点在 edge 行内联定义，单独节点单独一行
	// 为简洁，直接在 edge 行内联所有节点标签
	edgeSet := make(map[string]bool)
	for _, e := range c.edges {
		edgeSet[e.From] = true
		edgeSet[e.To] = true
	}

	// 先输出所有边（内联节点定义）
	for _, e := range c.edges {
		fromStep := c.findStep(e.From)
		toStep := c.findStep(e.To)
		if fromStep == nil || toStep == nil {
			continue
		}
		if e.Label != "" {
			fmt.Fprintf(&sb, "  %s[\"%s\"] -->|%s| %s[\"%s\"]\n",
				e.From, mmdLabel(fromStep), escapeMmd(e.Label),
				e.To, mmdLabel(toStep))
		} else {
			fmt.Fprintf(&sb, "  %s[\"%s\"] --> %s[\"%s\"]\n",
				e.From, mmdLabel(fromStep),
				e.To, mmdLabel(toStep))
		}
	}

	// 输出没有参与任何边的孤立节点
	for _, s := range c.steps {
		if !edgeSet[s.NodeID] {
			fmt.Fprintf(&sb, "  %s[\"%s\"]\n", s.NodeID, mmdLabel(&s))
		}
	}

	// 节点样式：失败=红，成功=绿，pending=默认灰
	for _, s := range c.steps {
		switch s.Status {
		case mmdStatusFailed:
			fmt.Fprintf(&sb, "  style %s fill:#d64,color:#fff\n", s.NodeID)
		case mmdStatusSuccess:
			fmt.Fprintf(&sb, "  style %s fill:#4a4,color:#fff\n", s.NodeID)
		}
	}

	return sb.String()
}

// Steps 返回当前步骤快照（只读副本）。
func (c *TaskMermaidCanvas) Steps() []MmdStep {
	c.mu.Lock()
	defer c.mu.Unlock()
	result := make([]MmdStep, len(c.steps))
	copy(result, c.steps)
	return result
}

// TokenEstimate 估算 Render() 输出的 token 数（4 字符 ≈ 1 token）。
func (c *TaskMermaidCanvas) TokenEstimate() int {
	r := c.Render()
	return len(r)/4 + 1
}

// Reset 清空画布（会话结束或强制重置时调用）。
func (c *TaskMermaidCanvas) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.steps = c.steps[:0]
	c.edges = c.edges[:0]
	c.pendingCalls = make(map[string]string)
	c.seq = 0
}

// NodeCount 返回已完成节点数。
func (c *TaskMermaidCanvas) NodeCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.steps)
}

// ─── 内部辅助 ─────────────────────────────────────────────────────────────────

func (c *TaskMermaidCanvas) findStep(nodeID string) *MmdStep {
	for i := range c.steps {
		if c.steps[i].NodeID == nodeID {
			return &c.steps[i]
		}
	}
	return nil
}

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
