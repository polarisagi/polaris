package surprise

import (
	"context"
	"sync"

	"github.com/polarisagi/polaris/internal/prompt/optimizer"
	"github.com/polarisagi/polaris/pkg/concurrent"
)

// SurpriseIndex — System 1/2 路由信号计算。
// 权威实现位于 M9（因依赖 MEMF），M3 仅暴露 Prometheus Gauge。
// 架构文档: docs/arch/M09-Self-Improvement-Engine.md §2.0

// DefaultLayerBThreshold Layer B 自动激活所需的默认转移次数。
// 对应 M09 §2.0 "500+ 成功轨迹"（平均每条轨迹 ~2 次转移）。
const DefaultLayerBThreshold float64 = 1000

// MinLayerBThreshold Layer B 阈值下限。
// 低于此值时 Laplace 先验主导信号，马尔可夫统计意义不足。
const MinLayerBThreshold float64 = 500

type SurpriseIndex struct {
	EmbeddingSurprise    float64
	ToolSequenceSurprise float64
	MEMFMatchSurprise    float64
}

// Compute 计算 SurpriseIndex = 0.4*embedding + 0.35*toolSeq + 0.25*MEMF.
func (si *SurpriseIndex) Compute() float64 {
	return 0.4*si.EmbeddingSurprise +
		0.35*si.ToolSequenceSurprise +
		0.25*si.MEMFMatchSurprise
}

// SurpriseCalculator 异步计算器 (BoundedWorkQueue + LoadShedder)
type SurpriseCalculator struct {
	queue           chan *CalcRequest
	memfPool        *optimizer.FallacyMemoryPool
	markov          *MarkovMatrix // 始终非 nil；达到 layerBThreshold 后自动激活 Layer B
	layerBThreshold float64       // 可配置激活阈值，默认 DefaultLayerBThreshold
	rollingAvg      float64       // 滑动平均 SurpriseIndex
	rollingCount    int64
	mu              sync.Mutex // 保护 rollingAvg/rollingCount/markov
	cancel          context.CancelFunc
}

// RoutingSource 标记触发本次计算的执行路径来源。
// 0 = 未知（兼容旧调用方）；1 = System 1（快速路径）；2 = System 2 / MCTS 探索。
// System 2 探索序列对马尔可夫矩阵的权重衰减为 0.3，防止探索偏置分布。
const (
	RoutingSourceUnknown int = 0
	RoutingSourceSystem1 int = 1
	RoutingSourceSystem2 int = 2
)

type CalcRequest struct {
	TaskID        string
	TaskType      string
	Keywords      []string // Embedding 的替代
	ToolSeq       []string // 工具序列
	RoutingSource int      // 执行路径来源，见 RoutingSource* 常量
	ResultCh      chan float64
}

// NewSurpriseCalculator 使用默认 Layer B 阈值（DefaultLayerBThreshold）构造计算器。
func NewSurpriseCalculator(memf *optimizer.FallacyMemoryPool) *SurpriseCalculator {
	return NewSurpriseCalculatorWith(memf, DefaultLayerBThreshold)
}

// NewSurpriseCalculatorWith 构造计算器，layerBThreshold 指定 Layer B 激活阈值。
// 低于 MinLayerBThreshold 的值会被自动修正为 MinLayerBThreshold。
func NewSurpriseCalculatorWith(memf *optimizer.FallacyMemoryPool, layerBThreshold float64) *SurpriseCalculator {
	if layerBThreshold < MinLayerBThreshold {
		layerBThreshold = MinLayerBThreshold
	}
	ctx, cancel := context.WithCancel(context.Background())
	c := &SurpriseCalculator{
		queue:           make(chan *CalcRequest, 256), // cap=256
		memfPool:        memf,
		markov:          NewMarkovMatrix(), // 始终初始化，持续积累数据
		layerBThreshold: layerBThreshold,
		cancel:          cancel,
	}
	for range 4 {
		concurrent.SafeGo(ctx, "surprise_calculator.worker_loop", func(ctx context.Context) {
			c.workerLoop(ctx)
		})
	}
	return c
}

// Close 停止所有 worker goroutine，释放资源。模块重启或测试结束时调用。
func (c *SurpriseCalculator) Close() {
	c.cancel()
}

// WithMarkovMatrix 替换内部马尔可夫矩阵（warm-start：从持久化数据恢复预训练矩阵）。
// 正常运行时无需调用；矩阵由 workerLoop 在线自动积累并在达到阈值后激活。
func (c *SurpriseCalculator) WithMarkovMatrix(m *MarkovMatrix) {
	c.mu.Lock()
	c.markov = m
	c.mu.Unlock()
}

// Submit 提交计算任务。如果队列满，执行丢弃降载（LoadShedding）。
func (c *SurpriseCalculator) Submit(req *CalcRequest) bool {
	select {
	case c.queue <- req:
		return true
	default:
		return false
	}
}

// SubmitToolSeq 实现了 consumer-side (L1) 定义的 SurpriseReader 接口的提交方法，
// 内部封装转换为 CalcRequest，打破包依赖循环。
func (c *SurpriseCalculator) SubmitToolSeq(taskID string, toolSeq []string) {
	c.Submit(&CalcRequest{
		TaskID:  taskID,
		ToolSeq: toolSeq,
	})
}

// CurrentSurprise 返回滑动平均 SurpriseIndex，实现 SurpriseReader 接口。
// 无历史数据时返回默认值 0.5。
func (c *SurpriseCalculator) CurrentSurprise() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rollingCount == 0 {
		return 0.5
	}
	return c.rollingAvg
}

func (c *SurpriseCalculator) workerLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case req, ok := <-c.queue:
			if !ok {
				return
			}
			c.processRequest(req)
		}
	}
}

func (c *SurpriseCalculator) processRequest(req *CalcRequest) {
	embSurprise := 0.2
	if len(req.Keywords) == 0 {
		embSurprise = 0.8
	}

	c.mu.Lock()
	markov := c.markov
	threshold := c.layerBThreshold
	c.mu.Unlock()

	// 无论 Layer B 是否激活，始终积累转移数据（先算再更新，避免自我影响）
	var toolSurprise float64
	if markov.TotalTransitions() >= threshold {
		// Layer B（M09 §2.0）：转移数达标，用马尔可夫条件概率惊异值
		toolSurprise = markov.Surprise(req.ToolSeq)
	} else {
		// Tier-0 基线：bash/computer_use 启发式加权
		toolSurprise = 0.1
		for _, t := range req.ToolSeq {
			if t == "bash" || t == "computer_use" {
				toolSurprise += 0.3
			}
		}
		if toolSurprise > 1.0 {
			toolSurprise = 1.0
		}
	}
	// System 2 / MCTS 探索序列权重衰减 0.3，避免探索偏置马尔可夫矩阵分布。
	// RoutingSourceUnknown（旧调用方兼容）视为全权重。
	markovWeight := 1.0
	if req.RoutingSource == RoutingSourceSystem2 {
		markovWeight = 0.3
	}
	markov.UpdateWeighted(req.ToolSeq, markovWeight)

	memfSurprise := 0.1
	if c.memfPool != nil {
		maxQuality := c.memfPool.GetMaxQualityScore(context.Background(), req.TaskType)
		if maxQuality > 0 {
			memfSurprise += maxQuality * 0.5
		}
	}
	if memfSurprise > 1.0 {
		memfSurprise = 1.0
	}

	idx := &SurpriseIndex{
		EmbeddingSurprise:    embSurprise,
		ToolSequenceSurprise: toolSurprise,
		MEMFMatchSurprise:    memfSurprise,
	}
	result := idx.Compute()

	// 维护滑动平均（EWMA α=0.2），mutex 保护并发读写
	c.mu.Lock()
	if c.rollingCount == 0 {
		c.rollingAvg = result
	} else {
		c.rollingAvg = 0.8*c.rollingAvg + 0.2*result
	}
	c.rollingCount++
	c.mu.Unlock()

	select {
	case req.ResultCh <- result:
	default:
	}
}
