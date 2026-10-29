package adapter

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"

	"github.com/polarisagi/polaris/pkg/concurrent"
)

// TrainingSampleCollector 累积 TrainingSample，达到 batchSize 后异步触发一次
// TrainingAdapter.Train（QLoRA/PRM 共用同一 Train(ctx, samples) 签名，见本文件
// 同包 training.go 的 TrainingAdapter interface，二者复用同一采集器类型）。
//
// 2026-07-21 deadcode 审查补齐：QLoRAAdapter.Train/PRMAdapter.Train 此前虽已
// 完整实现（HTTP POST 训练服务），但全仓库没有任何生产代码产出 TrainingSample
// 并决定触发时机——本类型是缺失的"样本累积 + 批次触发"编排层。样本来源与
// 触发时机的判定：
//   - QLoRA：内接 internal/learning/reflexion.ReflexionEngine.replaySuccess
//     （"经 replan 后成功"的纠偏轨迹，真实存在的高质量数据，非臆测）。
//   - PRM：内接 internal/eval/analysis 的 M12 §9 生产流量采样 LLM Judge 打分
//     （[0,1] 质量分作为 Reward，真实存在的评判信号，非臆测）。
//
// 触发是尽力而为语义：训练服务不可用时记录日志丢弃本批次样本，不重试、不回填
// ——训练服务本身是后台慢速自演化的一部分，不应因暂时不可用而阻塞或累积无界
// 内存增长。
type TrainingSampleCollector struct {
	mu        sync.Mutex
	samples   []protocol.TrainingSample
	adapter   protocol.TrainingAdapter
	batchSize int
	name      string // 日志标识（"qlora"/"prm"）
}

// NewTrainingSampleCollector 创建采集器。a 为 nil 时 Add 直接跳过（nil-safe，
// 对应 FeatureQLoRA/FeaturePRMTraining 未启用场景）。batchSize<=0 时使用默认 64。
func NewTrainingSampleCollector(name string, a protocol.TrainingAdapter, batchSize int) *TrainingSampleCollector {
	if batchSize <= 0 {
		batchSize = 64
	}
	return &TrainingSampleCollector{adapter: a, batchSize: batchSize, name: name}
}

// Add 追加一条样本；达到 batchSize 时异步触发一次训练并清空缓冲区。
// c 为 nil 或未注入 adapter 时安全跳过，调用方无需先判空。
func (c *TrainingSampleCollector) Add(sample protocol.TrainingSample) {
	if c == nil || c.adapter == nil {
		return
	}
	c.mu.Lock()
	c.samples = append(c.samples, sample)
	var batch []protocol.TrainingSample
	if len(c.samples) >= c.batchSize {
		batch = c.samples
		c.samples = nil
	}
	c.mu.Unlock()

	if batch == nil {
		return
	}
	name, trainer := c.name, c.adapter
	concurrent.SafeGo(context.Background(), name+"-train-trigger", func(ctx context.Context) {
		gctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if _, err := trainer.Train(gctx, batch); err != nil {
			slog.Warn(name+": training trigger failed", "err", err, "batch_size", len(batch))
		}
	})
}

// Pending 返回当前缓冲区大小（观测用，非热路径；nil-safe）。
func (c *TrainingSampleCollector) Pending() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.samples)
}
