package surprise

import (
	"context"
	"log/slog"
	"time"

	"github.com/polarisagi/polaris/pkg/concurrent"
)

// DriftOrchestrator 周期性驱动 M05 §12.3 漂移响应编排：
// DetectByTaskType() → 按 task_type 同步 DriftDowngradeRegistry 降级状态 →
// 存在新增降级时尽力触发一次后台重嵌批次。
//
// 放在 internal/learning/surprise（而非 internal/memory/retrieval）：架构分层
// R1.7 要求依赖单向 L0←L1←L2←L3，internal/memory 属 L1、internal/learning 属
// L2，L1 禁止反向 import L2。reindex 触发闭包只是裸函数类型（无需 import
// retrieval.OnlineReindexer 具体类型），本类型放在 L2 不产生任何跨层依赖；
// HybridRetrieverImpl 侧则反过来在 retrieval 包内本地声明消费方接口
// （AnchorRecorder/DriftGate，见 retriever_construct.go），由 cmd/polaris
// 组合根注入满足这些接口的 *DriftDetector/*DriftDowngradeRegistry 实例。
//
// 2026-07-21 deadcode 审查补齐：DriftDetector/EmbeddingVersionTracker 此前
// 全套实现完整但零生产调用点，本类型是缺失的"周期性驱动"编排层。
type DriftOrchestrator struct {
	detector *DriftDetector
	registry *DriftDowngradeRegistry
	reindex  func(context.Context) (processed int, remaining bool, err error) // 可为 nil（Embedder 未启用时）
	interval time.Duration
}

// NewDriftOrchestrator 创建漂移响应编排器。
// reindexTrigger 复用 cmd/polaris.startOnlineReindexer 返回的同一闭包
// （ModelRegistry.DeprecateModel 已在用这套"尽力而为触发一次批次"模式）；
// 为 nil 时仍会正常更新降级状态，只是不会主动触发重嵌批次。
func NewDriftOrchestrator(
	detector *DriftDetector,
	registry *DriftDowngradeRegistry,
	reindexTrigger func(context.Context) (int, bool, error),
	interval time.Duration,
) *DriftOrchestrator {
	return &DriftOrchestrator{
		detector: detector,
		registry: registry,
		reindex:  reindexTrigger,
		interval: interval,
	}
}

// RunOnce 执行一轮检测并同步降级状态。
//
// 降级解除语义：每轮都用当前 anchor 窗口重新评估 NeedsReindex 并直接覆盖
// registry 状态（而非"触发一次 Run() 后就假设已修复"）。原因：OnlineReindexer.Run
// 单次调用只处理一批（batchSize=50）且只在 embed_model_version 版本不匹配时
// 才有实际效果——若此刻并无新模型版本，Run() 会立即返回 remaining=false，
// 但这不代表 anchor 揭示的向量空间漂移已被修正。真正的解除依据是下一轮
// Detect 用新鲜 anchor 重新评分后 NeedsReindex 变为 false，避免虚假宣布恢复
// （R1：不臆测"重嵌已完成"这一无法从现有信号直接验证的结论）。
func (o *DriftOrchestrator) RunOnce(ctx context.Context) {
	if o.detector == nil || o.registry == nil {
		return
	}
	reports := o.detector.DetectByTaskType()

	anyNeedsReindex := false
	for taskType, report := range reports {
		o.registry.SetDowngraded(taskType, report.NeedsReindex)
		if report.NeedsReindex {
			anyNeedsReindex = true
			slog.Warn("drift_orchestrator: task_type downgraded to BM25-only due to embedding drift",
				"task_type", taskType, "change_rate", report.ChangeRate, "cosine_delta", report.CosineDelta)
		} else if report.UnknownTaskTypeAlarm {
			slog.Warn("drift_orchestrator: high unknown-expected ratio in anchor sample",
				"task_type", taskType, "unknown_ratio", report.UnknownRatio)
		}
	}

	if anyNeedsReindex && o.reindex != nil {
		if _, _, err := o.reindex(ctx); err != nil {
			slog.Warn("drift_orchestrator: reindex trigger failed", "err", err)
		}
	}
}

// Start 启动周期性检测 goroutine（interval<=0 时使用 §12.3 默认 168h）。
func (o *DriftOrchestrator) Start(ctx context.Context) {
	interval := o.interval
	if interval <= 0 {
		interval = 168 * time.Hour
	}
	concurrent.SafeGo(ctx, "drift-orchestrator", func(ctx context.Context) {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				o.RunOnce(ctx)
			}
		}
	})
	slog.Info("polaris: drift orchestrator started", "interval", interval.String())
}
