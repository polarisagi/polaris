package skill

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// evolutionCooldown 同一技能两次触发 Logic Collapse 重编译之间的最短间隔。
// 2026-07-04 审计修复（Task 10）：CheckAndEvolve 由 ConsolidationPipeline stage5
// 在每个会话的每轮 consolidation 中调用，无冷却时会对同一低成功率技能重复触发编译。
const evolutionCooldown = 1 * time.Hour

// SkillEvolutionMonitor 定期扫描低成功率的技能并触发演化。
type SkillEvolutionMonitor struct {
	db       protocol.SQLQuerier
	registry protocol.SkillRegistry
	compiler *LogicCollapseCompiler // 复用蒸馏管线
	cfg      *config.Thresholds

	mu              sync.Mutex
	running         bool                 // 防止并发 CheckAndEvolve 重复全表扫描（多会话可能同时触发 consolidation）
	lastTriggeredAt map[string]time.Time // 按 skillName 记录上次触发编译的时间，用于冷却期判定
}

// NewSkillEvolutionMonitor 创建演化监控器。
func NewSkillEvolutionMonitor(db protocol.SQLQuerier, registry protocol.SkillRegistry, compiler *LogicCollapseCompiler, cfg *config.Thresholds) *SkillEvolutionMonitor {
	return &SkillEvolutionMonitor{
		db:              db,
		registry:        registry,
		compiler:        compiler,
		cfg:             cfg,
		lastTriggeredAt: make(map[string]time.Time),
	}
}

// CheckAndEvolve 周期性扫描 episodic_events 中的工具调用事件，
// 计算技能成功率，对于低于阈值的技能触发 Logic Collapse 重生成。
func (m *SkillEvolutionMonitor) CheckAndEvolve(ctx context.Context) error {
	if m.cfg == nil {
		return nil
	}
	threshold := m.cfg.M6Skill.EvolutionSuccessThreshold
	minUsage := m.cfg.M6Skill.EvolutionMinUsage

	if threshold <= 0 || minUsage <= 0 {
		return nil // 阈值未配置
	}

	m.mu.Lock()
	if m.running {
		m.mu.Unlock()
		return nil // 已有一轮扫描在执行（多会话并发触发 consolidation），跳过本次
	}
	m.running = true
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.running = false
		m.mu.Unlock()
	}()

	skillStats, err := m.gatherSkillStats(ctx)
	if err != nil {
		return err
	}

	m.triggerEvolutions(ctx, skillStats, threshold, minUsage)
	return nil
}

type skillStatEntry struct {
	Total   int
	Success int
}

func (m *SkillEvolutionMonitor) gatherSkillStats(ctx context.Context) (map[string]*skillStatEntry, error) {
	// 2026-07-04 审计修复（Task 10）：event_type 原值 'tool_result' 在系统中从未被写入
	// （实际写入值见 internal/protocol/pb/event.pb.go EventType 注释 与
	// internal/memory/consolidation 投影逻辑，工具调用事件统一写作 'tool_call'，
	// content 内 JSON 含 tool_name/success 字段），此前该查询恒为空结果集，
	// CheckAndEvolve 从未真正触发过技能演化。
	query := `
		SELECT content
		FROM episodic_events
		WHERE event_type = 'tool_call' AND archived = 0
	`
	rows, err := m.db.QueryContext(ctx, query)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "query episodic_events failed", err)
	}
	defer rows.Close()

	skillStats := make(map[string]*skillStatEntry)

	for rows.Next() {
		var content string
		if err := rows.Scan(&content); err != nil {
			continue
		}
		var payload struct {
			ToolName string `json:"tool_name"`
			Name     string `json:"name"`
			Success  bool   `json:"success"`
		}
		if err := json.Unmarshal([]byte(content), &payload); err != nil {
			continue
		}
		name := payload.ToolName
		if name == "" {
			name = payload.Name
		}
		// 我们只关注 skill
		if !strings.HasPrefix(name, types.SkillPrefix) && !strings.HasPrefix(name, "auto_") {
			continue
		}
		if _, ok := skillStats[name]; !ok {
			skillStats[name] = &skillStatEntry{}
		}
		skillStats[name].Total++
		if payload.Success {
			skillStats[name].Success++
		}
	}
	return skillStats, nil
}

func (m *SkillEvolutionMonitor) triggerEvolutions(ctx context.Context, skillStats map[string]*skillStatEntry, threshold float64, minUsage int) {
	now := time.Now()
	for skillName, stats := range skillStats {
		if stats.Total < minUsage {
			continue
		}
		rate := float64(stats.Success) / float64(stats.Total)
		if rate < threshold {
			m.mu.Lock()
			last, seen := m.lastTriggeredAt[skillName]
			if seen && now.Sub(last) < evolutionCooldown {
				m.mu.Unlock()
				slog.Debug("skill_evolution: skill still in cooldown, skip re-trigger", "skill", skillName, "since", last)
				continue
			}
			m.lastTriggeredAt[skillName] = now
			m.mu.Unlock()

			slog.InfoContext(ctx, "Skill success rate below threshold, triggering Logic Collapse", "skill", skillName, "rate", rate, "total", stats.Total)

			// 触发 Logic Collapse (此处简单构造 CompileRequest，因为是复用管线)
			// 注意：真正的蒸馏需要原始 Trajectory，目前仅发送编译请求
			req := &CompileRequest{
				Trajectory: &CollapseTrajectory{
					SkillID:    skillName,
					TaintLevel: 1, // TaintLow
				},
			}
			if m.compiler != nil {
				if _, err := m.compiler.Compile(ctx, req); err != nil {
					slog.WarnContext(ctx, "Logic Collapse compile failed during evolution", "skill", skillName, "err", err)
				}
			}
		}
	}
}
