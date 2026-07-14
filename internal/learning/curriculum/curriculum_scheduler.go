package curriculum

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/llm/safecall"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/concurrent"
	"github.com/polarisagi/polaris/pkg/types"
)

// llmJudgeSafe LLM-as-Judge 安全审查（Tier1+）。
// 调用 LLM 判断任务描述是否安全：返回 "SAFE"/"UNSAFE"。
// 超时或 LLM 错误时 fail-closed（安全优先）。
func (ag *AutoCurriculumGenerator) llmJudgeSafe(ctx context.Context, desc string) bool {
	judgeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	prompt := fmt.Sprintf(
		"Safety check for AI training task: %q\n"+
			"Reply with exactly one word: SAFE or UNSAFE.\n"+
			"UNSAFE if the task involves: hacking, self-modification, data deletion, deception, or harm.",
		desc,
	)
	req := &types.InferRequest{
		Messages:    []types.Message{{Role: "user", Content: prompt}},
		MaxTokens:   8,
		Temperature: 0,
	}
	resp, err := safecall.Infer(judgeCtx, ag.llmProvider, req.Messages, types.WithMaxTokens(req.MaxTokens))
	if err != nil || resp == nil {
		slog.Warn("curriculum: llm_judge_safe error, fail-closed",
			"err", err,
			"action", "rejecting curriculum sample to prevent safety bypass",
		)
		return false // fail-closed：LLM 不可达时拒绝，防安全旁路
	}
	verdict := strings.TrimSpace(strings.ToUpper(resp.Content))
	safe := !strings.HasPrefix(verdict, "UNSAFE")
	if !safe {
		slog.Warn("curriculum: llm_judge rejected sample",
			"audit_event", "curriculum_hazard_log",
			"verdict", verdict,
			"desc_prefix", truncateStr(desc, 80),
		)
	}
	return safe
}

// isFrozen 检查技能是否处于冻结期。
func (ag *AutoCurriculumGenerator) isFrozen(skill string) bool {
	ag.mu.Lock()
	defer ag.mu.Unlock()
	if ag.globalFreeze {
		return true
	}
	if t, ok := ag.frozenUntil[skill]; ok && time.Now().Before(t) {
		return true
	}
	return false
}

// SetGlobalFreeze sets the global freeze state.
func (ag *AutoCurriculumGenerator) SetGlobalFreeze(frozen bool) {
	ag.mu.Lock()
	defer ag.mu.Unlock()
	ag.globalFreeze = frozen
}

// RedTeamRunner 执行红队演练并注入到 Holdout 评估集（解耦依赖）。
type RedTeamRunner interface {
	RunAndInject(ctx context.Context) error
}

// BackgroundTaskScheduler 后台调度器。
//
// 2026-07-14 删除 foundingAnchor/trajectoryStore 字段 + FoundingAnchorMeta 接口 +
// InjectFoundingAnchor/InjectTrajectoryStore 方法：deadcode 复核确认三者在全仓库
// 范围内从未被 Inject（生产/测试均无调用点），且 FoundingAnchorMeta 声明的
// CompareWithAnchor(trajectories []types.Trajectory) float64 与
// eval.CompareWithAnchor(anchor *FoundingAnchor, fingerprint BehaviorFingerprint)
// DriftReport 参数/返回类型完全不匹配，eval.FoundingAnchor 结构性无法满足此接口——
// 是一次从未跑通过的半成品设计，非"忘了注入"的简单遗漏。真正生效的 FoundingAnchor
// 漂移检测走 cmd/polaris/boot_agent.go 的 founding-anchor-drift-detector goroutine，
// 直接调用 eval.LoadOrCreate/ComputeFingerprint/CompareWithAnchor，不经过本调度器。
// eval.FoundingAnchor.GetCreatedAt/GetTaskCount 两个仅为满足本接口而存在的方法
// 一并删除（internal/eval/founding_anchor.go）。
type BackgroundTaskScheduler struct {
	generator      *AutoCurriculumGenerator
	bb             protocol.Blackboard
	surpriseReader SurpriseReader
	redTeam        RedTeamRunner        // 可选；nil 时跳过 24h 红队探测
	auditLogger    protocol.AuditLogger // 可 nil，nil 时降级为 slog.Error
	immuneGateway  immuneGatewayInterface
}

type immuneGatewayInterface interface {
	Scan(ctx context.Context, agentID string, scanType string) (any, error)
}

// SurpriseReader 读取当前系统 SurpriseIndex。
type SurpriseReader interface {
	CurrentSurprise() float64
}

// NewBackgroundTaskScheduler 创建后台调度器。
func NewBackgroundTaskScheduler(gen *AutoCurriculumGenerator, bb protocol.Blackboard) *BackgroundTaskScheduler {
	return &BackgroundTaskScheduler{generator: gen, bb: bb}
}

// InjectAuditLogger 注入审计日志记录器。
func (b *BackgroundTaskScheduler) InjectAuditLogger(logger protocol.AuditLogger) {
	b.auditLogger = logger
}

// InjectSurpriseReader 注入 SurpriseIndex 读取器（可选——nil 时使用 0.5 默认值）。
func (b *BackgroundTaskScheduler) InjectSurpriseReader(r SurpriseReader) {
	b.surpriseReader = r
}

// InjectImmuneGateway 注入免疫网关。
func (b *BackgroundTaskScheduler) InjectImmuneGateway(gateway immuneGatewayInterface) {
	b.immuneGateway = gateway
}

// InjectRedTeamProtocol 注入 Red Team 协议（可选）。
func (b *BackgroundTaskScheduler) InjectRedTeamProtocol(r RedTeamRunner) {
	b.redTeam = r
}

// readSurprise 读取当前系统 SurpriseIndex。
// 优先级: surpriseReader → 0.5 默认值。
func (b *BackgroundTaskScheduler) readSurprise() float64 {
	if b.surpriseReader != nil {
		return b.surpriseReader.CurrentSurprise()
	}
	return 0.5
}

// Start 启动后台守护协程（2 分钟轮询）。
func (b *BackgroundTaskScheduler) Start(ctx context.Context) {
	// 保持原有：2 分钟 AutoCurriculum 生成（不修改）
	concurrent.SafeGo(ctx, "curriculum-auto-generate", func(ctx context.Context) {
		ticker := time.NewTicker(2 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				si := b.readSurprise()
				b.generator.Generate(ctx, b.bb, si)
			}
		}
	})

	// 新增：7 天 FoundingAnchor 漂移检查（V8-S3）
	if b.immuneGateway != nil {
		concurrent.SafeGo(ctx, "curriculum-founding-anchor-check", func(ctx context.Context) {
			ticker := time.NewTicker(7 * 24 * time.Hour)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					// Delegate checking to M9 ImmuneGateway
					res, err := b.immuneGateway.Scan(ctx, "system", "founding_anchor")
					if err == nil && res != nil {
						if frozen, ok := res.(bool); ok && frozen {
							b.generator.SetGlobalFreeze(true)
						}
					}
				}
			}
		})
	}

	// 新增：24 小时 Red Team 常态化探测（V8-S1）
	if b.redTeam != nil {
		concurrent.SafeGo(ctx, "curriculum-red-team-probe", func(ctx context.Context) {
			ticker := time.NewTicker(24 * time.Hour)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if err := b.redTeam.RunAndInject(ctx); err != nil {
						slog.Error("red team: run and inject failed", "err", err)
					}
				}
			}
		})
	}
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
