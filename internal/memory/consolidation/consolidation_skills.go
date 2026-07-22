package consolidation

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/polarisagi/polaris/pkg/types"
)

// updateSkills 从成功的工具调用事件中提炼并注册技能（Logic Collapse）。
// 触发条件: 同一 tool_name 在 session 中成功调用 ≥ 3 次。
func (p *ConsolidationPipeline) updateSkills(
	ctx context.Context,
	_ string, // sessionID 保留用于未来的溯源追踪
	events []types.ScoredEvent,
) error {
	if p.skills == nil {
		return nil
	}

	// 统计 tool_call 类型事件的工具名出现次数
	toolCounts := make(map[string]int)
	for _, se := range events {
		if (func() *types.Event {
			if e, _ := se.Event.(*types.Event); e != nil {
				return e
			}
			return &types.Event{}
		}()).Type != "tool_result" && (func() *types.Event {
			if e, _ := se.Event.(*types.Event); e != nil {
				return e
			}
			return &types.Event{}
		}()).Type != "tool_call" {
			continue
		}
		// 从 payload 中提取 tool_name
		var payload struct {
			ToolName string `json:"tool_name"`
			Name     string `json:"name"`
			Success  bool   `json:"success"`
		}
		if err := json.Unmarshal((func() *types.Event {
			if e, _ := se.Event.(*types.Event); e != nil {
				return e
			}
			return &types.Event{}
		}()).Payload, &payload); err != nil {
			continue
		}
		name := payload.ToolName
		if name == "" {
			name = payload.Name
		}
		if name == "" || !payload.Success {
			continue
		}
		toolCounts[name]++
	}

	// 出现 ≥ 3 次的工具提炼为 Skill
	for toolName, count := range toolCounts {
		if count < 3 {
			continue
		}
		meta := types.SkillMeta{
			Name:         "auto_" + toolName,
			Version:      fmt.Sprintf("1.0.%d", time.Now().Unix()),
			Runtime:      "builtin",
			RiskLevel:    "low",
			Sandbox:      1,
			Capabilities: []string{toolName},
			ExecMode:     "tool",
			Trust:        types.TrustTier(1),
			Idempotent:   true,
		}
		if err := p.skills.Register(ctx, meta); err != nil {
			slog.Warn("consolidation: skills.Register failed", "err", err)
		}
	}
	return nil
}

// ─── Stage 3.5 ───────────────────────────────────────────────────────────────
