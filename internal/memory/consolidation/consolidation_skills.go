package consolidation

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/polarisagi/polaris/pkg/types"
)

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

// synthesizeUserProfile 从 Episodic 事件合成用户画像（L3 Persona）。
//
// 触发策略: events ≥ 10 且距上次合成 > 50 事件（由 LastEventTS 间接判断）。
// 来源: supermemory User Profile + TencentDB L3 Persona 收敛方案。
//
// LLM 路径: provider 非 nil → 用 100 token prompt 让 LLM 归纳 JSON。
// 规则 fallback: provider 为 nil → 统计工具频率 + 收集近期摘要。
