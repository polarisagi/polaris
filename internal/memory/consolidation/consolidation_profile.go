package consolidation

import (
	"context"
	"encoding/json"
	"maps"
	"strings"

	"github.com/polarisagi/polaris/internal/prompt/templates"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// synthesizeUserProfile 从 Episodic 事件合成用户画像（L3 Persona）。
//
// 触发策略: events ≥ 10 且距上次合成 > 50 事件（由 LastEventTS 间接判断）。
// 来源: supermemory User Profile + TencentDB L3 Persona 收敛方案。
//
// LLM 路径: provider 非 nil → 用 100 token prompt 让 LLM 归纳 JSON。
// 规则 fallback: provider 为 nil → 统计工具频率 + 收集近期摘要。
func (p *ConsolidationPipeline) synthesizeUserProfile(
	ctx context.Context,
	events []types.ScoredEvent,
) error {
	if p.semantic == nil {
		return nil
	}

	// 读取现有画像（确定是否需要更新）
	current, _ := p.semantic.GetUserProfile(ctx, "default")

	// 若 events 最新时间戳距上次合成 < 1 分钟，跳过（防重复合成）
	if current != nil && len(events) > 0 {
		newestTS := (func() *types.Event {
			if e, _ := events[0].Event.(*types.Event); e != nil {
				return e
			}
			return &types.Event{}
		}()).CreatedAt.UnixMilli()
		for _, se := range events {
			if ts := (func() *types.Event {
				if e, _ := se.Event.(*types.Event); e != nil {
					return e
				}
				return &types.Event{}
			}()).CreatedAt.UnixMilli(); ts > newestTS {
				newestTS = ts
			}
		}
		if newestTS-current.LastEventTS < 60_000 {
			return nil
		}
	}

	// 收集最新 event 时间戳
	var latestTS int64
	for _, se := range events {
		if ts := (func() *types.Event {
			if e, _ := se.Event.(*types.Event); e != nil {
				return e
			}
			return &types.Event{}
		}()).CreatedAt.UnixMilli(); ts > latestTS {
			latestTS = ts
		}
	}

	profile := types.UserProfile{
		ProfileKey:         "default",
		StableFacts:        make(map[string]any),
		BehavioralPatterns: make(map[string]any),
		LastEventTS:        latestTS,
	}
	if current != nil {
		profile.SynthesisCount = current.SynthesisCount + 1
		// 保留已有稳定事实（不被规则覆盖）
		maps.Copy(profile.StableFacts, current.StableFacts)
	}

	if p.summarizer != nil {
		p.llmSynthesizeProfile(ctx, current, events, &profile)
	} else {
		p.ruleSynthesizeProfile(events, &profile)
	}

	return p.semantic.UpsertUserProfile(ctx, profile) //nolint:wrapcheck
}

// llmSynthesizeProfile 通过 LLM 合成用户画像（100 token prompt，输出 JSON）。
//
//nolint:gocyclo
func (p *ConsolidationPipeline) llmSynthesizeProfile(
	ctx context.Context,
	current *types.UserProfile,
	events []types.ScoredEvent,
	out *types.UserProfile,
) {
	// 组装最近 15 条事件文本
	var sb strings.Builder
	limit := min(15, len(events))
	for _, se := range events[:limit] {
		sb.WriteString(string((func() *types.Event {
			if e, _ := se.Event.(*types.Event); e != nil {
				return e
			}
			return &types.Event{}
		}()).Type))
		sb.WriteString(": ")
		payload := string((func() *types.Event {
			if e, _ := se.Event.(*types.Event); e != nil {
				return e
			}
			return &types.Event{}
		}()).Payload)
		if len(payload) > 100 {
			payload = payload[:100]
		}
		sb.WriteString(payload)
		sb.WriteByte('\n')
	}

	currentJSON := "{}"
	if current != nil {
		if b, err := json.Marshal(current); err == nil {
			currentJSON = string(b)
		}
	}

	promptText, err := templates.Render("user_profile_synthesis.tmpl", map[string]any{
		"CurrentJSON": currentJSON,
		"Events":      sb.String(),
	})
	if err != nil {
		p.ruleSynthesizeProfile(events, out)
		return
	}

	respContent, err := p.summarizer.InferRaw(ctx, promptText, 512)
	if err != nil {
		p.ruleSynthesizeProfile(events, out)
		return
	}

	content := strings.TrimSpace(respContent)
	if idx := strings.Index(content, "{"); idx > 0 {
		content = content[idx:]
	}
	if idx := strings.LastIndex(content, "}"); idx >= 0 && idx < len(content)-1 {
		content = content[:idx+1]
	}

	var parsed struct {
		StableFacts        map[string]any `json:"stable_facts"`
		RecentActivity     []string       `json:"recent_activity"`
		BehavioralPatterns map[string]any `json:"behavioral_patterns"`
	}
	if err := json.Unmarshal([]byte(content), &parsed); err != nil {
		p.ruleSynthesizeProfile(events, out)
		return
	}
	if parsed.StableFacts != nil {
		out.StableFacts = parsed.StableFacts
	}
	if len(parsed.RecentActivity) > 0 {
		out.RecentActivity = parsed.RecentActivity
	}
	if parsed.BehavioralPatterns != nil {
		out.BehavioralPatterns = parsed.BehavioralPatterns
	}
}

// ruleSynthesizeProfile 规则 fallback：统计工具频率 + 收集近期事件摘要。
func (p *ConsolidationPipeline) ruleSynthesizeProfile(
	events []types.ScoredEvent,
	out *types.UserProfile,
) {
	toolFreq := make(map[string]int)
	eventTypFreq := make(map[string]int)
	var recentSummaries []string

	for _, se := range events {
		eventTypFreq[string((func() *types.Event {
			if e, _ := se.Event.(*types.Event); e != nil {
				return e
			}
			return &types.Event{}
		}()).Type)]++

		var payload struct {
			ToolName string `json:"tool_name"`
			Name     string `json:"name"`
		}
		if err := json.Unmarshal((func() *types.Event {
			if e, _ := se.Event.(*types.Event); e != nil {
				return e
			}
			return &types.Event{}
		}()).Payload, &payload); err == nil {
			name := payload.ToolName
			if name == "" {
				name = payload.Name
			}
			if name != "" {
				toolFreq[name]++
			}
		}

		// 收集近期摘要（最多 20 条）
		if len(recentSummaries) < 20 && len((func() *types.Event {
			if e, _ := se.Event.(*types.Event); e != nil {
				return e
			}
			return &types.Event{}
		}()).Payload) > 0 {
			summary := string((func() *types.Event {
				if e, _ := se.Event.(*types.Event); e != nil {
					return e
				}
				return &types.Event{}
			}()).Payload)
			if len(summary) > 80 {
				summary = summary[:80]
			}
			recentSummaries = append(recentSummaries, string((func() *types.Event {
				if e, _ := se.Event.(*types.Event); e != nil {
					return e
				}
				return &types.Event{}
			}()).Type)+": "+summary)
		}
	}

	out.BehavioralPatterns["tool_frequency"] = toolFreq
	out.BehavioralPatterns["event_type_frequency"] = eventTypFreq
	out.RecentActivity = recentSummaries
}

// ============================================================================
// Forgetting — 双层策略（热删除 + 冷归档）
// 架构文档: docs/arch/M05-Memory-System.md §5

// ForgettingManager 遗忘管理器。
// 热删除: Q-Learning 效用衰减 → DecayWeight < salienceThreshold → Forgettable.
// 冷归档: Forgettable + age > 30d → 归档 + tombstone.
// store 用于持久化操作（扫描事件、写入归档标记）。
type ForgettingManager struct {
	store             protocol.Store
	cognitive         protocol.CognitiveSearcher
	decayRate         float64 // 0.01/日
	salienceThreshold float64

	archiver *ColdArchiver
}
