package reflexion

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/guard"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// ============================================================================
// ReflectionWorker
// 架构文档: docs/arch/M05-Memory-System.md §3.4
// ============================================================================

// ReflectionConfig 控制 ReflectionWorker 的触发策略。
// 通过 NewReflectionWorkerWithConfig 注入；零值等效于默认白名单。
type ReflectionConfig struct {
	// TaskTypeWhitelist 允许触发反思的任务类型集合。空 slice 等效默认集合。
	TaskTypeWhitelist []string
	// MinReplanCount 低于此重规划次数时，白名单外的任务类型跳过反思（默认 2）。
	MinReplanCount int
}

// defaultReflectionConfig 返回出厂默认值，可通过 configs/defaults.toml 覆盖。
func defaultReflectionConfig() ReflectionConfig {
	return ReflectionConfig{
		TaskTypeWhitelist: []string{"complex_reasoning", "coding", "research", "debug", "analysis"},
		MinReplanCount:    2,
	}
}

// ReflectionWorker 后台协程，负责在任务结束或 Session 关闭时提取 Agent 反思记忆。
type ReflectionWorker struct {
	episodic   protocol.EpisodicMemory
	provider   protocol.Provider
	reflection protocol.ReflectionMemory
	cfg        ReflectionConfig
}

func NewReflectionWorker(episodic protocol.EpisodicMemory, provider protocol.Provider, reflection protocol.ReflectionMemory) *ReflectionWorker {
	return &ReflectionWorker{
		episodic:   episodic,
		provider:   provider,
		reflection: reflection,
		cfg:        defaultReflectionConfig(),
	}
}

// NewReflectionWorkerWithConfig 创建可配置触发策略的 ReflectionWorker。
func NewReflectionWorkerWithConfig(episodic protocol.EpisodicMemory, provider protocol.Provider, reflection protocol.ReflectionMemory, cfg ReflectionConfig) *ReflectionWorker {
	def := defaultReflectionConfig()
	if len(cfg.TaskTypeWhitelist) == 0 {
		cfg.TaskTypeWhitelist = def.TaskTypeWhitelist
	}
	if cfg.MinReplanCount <= 0 {
		cfg.MinReplanCount = def.MinReplanCount
	}

	return &ReflectionWorker{
		episodic:   episodic,
		provider:   provider,
		reflection: reflection,
		cfg:        cfg,
	}
}

// ConsolidateReflections 在任务终态触发。
// success=false 的任务无条件反思（失败是最有价值的反思素材）；
// success=true 时按白名单 + 重规划次数门控（避免平凡成功任务浪费 LLM token）。
//
//nolint:gocyclo
func (rw *ReflectionWorker) ConsolidateReflections(ctx context.Context, taskID string, taskType string, replanCount int, success bool) error {
	if success && replanCount < rw.cfg.MinReplanCount && !rw.isWhitelisted(taskType) {
		return nil
	}

	// 1. 收集 Evidence Episodic Events
	events, err := rw.episodic.Query(ctx, types.EpisodicQuery{
		SessionID:     taskID,
		K:             100,
		MaxTaintLevel: types.TaintNone, // 系统内部，显式 TaintNone
	})
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "reflection_worker: query episodic events", err)
	}
	if len(events) == 0 {
		return nil
	}

	// 2. LLM 提取反思
	var sb strings.Builder
	eventIDs := make([]string, 0, len(events))
	for _, se := range events {
		eventIDs = append(eventIDs, (func() *types.Event {
			if e, _ := se.Event.(*types.Event); e != nil {
				return e
			}
			return &types.Event{}
		}()).ID)
		sb.WriteString(string((func() *types.Event {
			if e, _ := se.Event.(*types.Event); e != nil {
				return e
			}
			return &types.Event{}
		}()).Type))
		sb.WriteString(": ")
		sb.WriteString(string((func() *types.Event {
			if e, _ := se.Event.(*types.Event); e != nil {
				return e
			}
			return &types.Event{}
		}()).Payload))
		sb.WriteByte('\n')
	}

	prompt := fmt.Sprintf(
		"Extract a metacognitive reflection from the following AI agent session. Focus on 'success_pattern', 'failure_mode', 'efficiency_insight', or 'cross_task_principle'.\n"+
			"Output strictly JSON:\n"+
			"{\n"+
			"  \"reflection_type\": \"...\",\n"+
			"  \"content\": \"... (max 500 tokens)\"\n"+
			"}\n\nSession Logs:\n%s",
		sb.String(),
	)

	//nolint:bare-infer // 历史代码暂留，后续重构替换
	resp, err := rw.provider.Infer(ctx, []types.Message{{Role: "user", Content: prompt}}, types.WithMaxTokens(512))
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "ReflectionWorker.ConsolidateReflections", err)
	}

	// 3. 解析结果
	content := strings.TrimSpace(resp.Content)
	if idx := strings.Index(content, "{"); idx > 0 {
		content = content[idx:]
	}
	if idx := strings.LastIndex(content, "}"); idx >= 0 && idx < len(content)-1 {
		content = content[:idx+1]
	}

	var res struct {
		ReflectionType string `json:"reflection_type"`
		Content        string `json:"content"`
	}
	if err := json.Unmarshal([]byte(content), &res); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "ReflectionWorker.ConsolidateReflections", err)
	}

	if res.Content == "" {
		return nil
	}

	// 4. M11 FactualityGuard 抽样核验
	fg := guard.NewFactualityGuard()
	if rw.provider != nil {
		fg.InjectLLMProvider(rw.provider)
	}
	verdict, errVerify := fg.Verify(ctx, res.Content, sb.String(), types.TaintLow)
	if errVerify != nil {
		slog.Warn("reflection_worker: factuality check returned error, skipping write", "err", errVerify)
		return nil
	}
	if verdict.Verdict == guard.FactualityFail {
		slog.Warn("reflection_worker: factuality check failed, skipping write")
		return nil
	}
	// 5. 写入 ReflectionMemory
	entry := types.ReflectionEntry{
		ID:         fmt.Sprintf("ref_%d", time.Now().UnixNano()),
		SessionID:  taskID,
		FailReason: "N/A",
		Strategy:   res.ReflectionType,
		Decision:   res.Content,
		Meta: map[string]any{
			"task_type":          taskType,
			"evidence_event_ids": eventIDs,
			"salience":           0.8,
			"taint_level":        types.TaintLow,
		},
		CreatedAt: time.Now(),
	}

	return rw.reflection.AppendReflection(ctx, entry)
}

// isWhitelisted 判断 taskType 是否在触发反思的白名单内。
func (rw *ReflectionWorker) isWhitelisted(taskType string) bool {
	for _, t := range rw.cfg.TaskTypeWhitelist {
		if t == taskType {
			return true
		}
	}
	return false
}
