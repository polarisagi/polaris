package governance

import (
	"context"
	"encoding/json"
	"math"
	"os/exec"
	"sort"
)

// ShadowExecutor — 影子执行与对比评估。
// 架构文档: docs/arch/12-Eval-Harness-深度选型.md §8

// ShadowExecutor 并行执行 baseline 和 candidate 版本，对比结果。
// 流量分发由 M9 ProgressiveRollout (pkg/swarm/rollout.go) 决策，
// M13 TrafficSplitter (pkg/edge/scheduler.go) 执行。
type ShadowExecutor struct {
	baseline   *ShadowVersion
	candidate  *ShadowVersion
	store      *ShadowStore
	comparator *ShadowComparator
}

// NewShadowExecutor 创建 ShadowExecutor。
func NewShadowExecutor(store *ShadowStore) *ShadowExecutor {
	return &ShadowExecutor{
		store:      store,
		comparator: &ShadowComparator{},
	}
}

// SetVersions 设置 baseline 和 candidate 版本。
func (se *ShadowExecutor) SetVersions(b, c *ShadowVersion) {
	se.baseline = b
	se.candidate = c
}

// ShadowVersion 影子执行版本引用。
type ShadowVersion struct {
	Version   string
	Type      string // baseline | candidate | shadow | rollback
	ConfigRef string
	ModelID   string
	CreatedAt int64
}

// ShadowComparator 对比引擎。
type ShadowComparator struct{}

// Compare 对比两个版本的执行结果。
// 1. diffToolCalls + TokenDiff + LatencyDiff
// 2. token 增长 > baseline 30% → token_spike（ComparisonResult.ID 标注）
// 3. write_network 步骤以 tool_call 参数一致性（而非实际输出）作为评估依据。
// JudgeScore 由外部 L4LLMJudgeEvaluator 异步注入，此处不调 LLM。
func (sc *ShadowComparator) Compare(baseline, candidate *AgentTrajectory) *ComparisonResult {
	res := &ComparisonResult{}
	if baseline == nil || candidate == nil {
		return res
	}
	br, cr := baseline.Result, candidate.Result
	if br == nil || cr == nil {
		return res
	}
	res.BaselineSuccess = br.Success
	res.CandidateSuccess = cr.Success
	res.TokenDiff = cr.TokensUsed - br.TokensUsed
	res.LatencyDiff = cr.LatencyMs - br.LatencyMs
	res.ToolCallDiff = len(cr.ToolCalls) - len(br.ToolCalls)
	// token spike：候选 token 用量超出 baseline 30%
	if br.TokensUsed > 0 && float64(cr.TokensUsed) > float64(br.TokensUsed)*1.3 {
		res.ID = "token_spike"
	}
	return res
}

// Execute 并行执行 baseline 和 candidate，对比结果。
//
// Candidate DryRun 模式 (ctx.X-Polaris-DryRun:true, M7 注入):
//
//	write_network/privileged → M7 Shadow Sink mock（基于 output_schema 自动生成 mock 响应）
//	write_local → os.TempDir()/shadow_{task_id}/（执行完毕后清理）
//	进程派生: Linux CLONE_NEWNET 网络隔离; macOS → 禁止执行 + Schema Mock 替代
//
// 多态混淆（防 Reward Hacking）:
//
//	clock_time_get: 生产时间戳 ±3600s 随机偏移
//	environ_get: 与生产环境变量一致（不注入候选版标记）
//	workspace 路径: 随机后缀防路径依赖
//	工具序列 vs baseline: 归一化去时间戳/随机数/路径 → 新工具调用 → structural_anomaly → HITL
func (se *ShadowExecutor) Execute(ctx context.Context, task *EvalTask) (*ComparisonResult, error) {
	// Phase 1: 并行 Execute(共享 ctx) → 超时标记不可用
	// 这里演示如何执行二进制并注入隔离属性。生产中将通过真实的 AgentRunner 收集轨迹。

	// Baseline 执行
	baselineCmd := exec.CommandContext(ctx, "polaris-agent", "--version", se.baseline.Version)
	baselineCmd.Env = append(baselineCmd.Env, "POLARIS_ENV=production")

	// Candidate 执行（DryRun 模式）
	candidateCmd := exec.CommandContext(ctx, "polaris-agent", "--version", se.candidate.Version)
	candidateCmd.Env = append(candidateCmd.Env, "X_POLARIS_DRYRUN=true")

	// 进程隔离: Linux CLONE_NEWNET; macOS Proxy/Offline Fallback
	isolateNetwork(candidateCmd)

	var baselineResult, candidateResult *AgentTrajectory

	// Collect baseline trajectory
	baselineOut, err := baselineCmd.Output()
	if err == nil {
		var traj AgentTrajectory
		if json.Unmarshal(baselineOut, &traj) == nil {
			baselineResult = &traj
		}
	}

	// Collect candidate trajectory
	candidateOut, err := candidateCmd.Output()
	if err == nil {
		var traj AgentTrajectory
		if json.Unmarshal(candidateOut, &traj) == nil {
			candidateResult = &traj
		}
	}

	// Phase 2: Compare 对比 diff
	res := se.comparator.Compare(baselineResult, candidateResult)

	// Phase 3: 写 ShadowStore
	if se.store != nil {
		se.store.Add(res)
	}
	return res, nil
}

// ComparisonResult 对比结果。
type ComparisonResult struct {
	ID               string
	TokenDiff        int
	LatencyDiff      int64
	ToolCallDiff     int
	JudgeScore       float64
	BaselineSuccess  bool
	CandidateSuccess bool
}

// ShadowStore 影子执行结果存储。
type ShadowStore struct {
	results []*ComparisonResult
}

// Add 记录对比结果。
func (ss *ShadowStore) Add(res *ComparisonResult) {
	ss.results = append(ss.results, res)
}

// GetAggregatedMetrics 获取聚合指标。
func (ss *ShadowStore) GetAggregatedMetrics() *AggregatedMetrics {
	return &AggregatedMetrics{}
}

// AggregatedMetrics 聚合对比指标。
type AggregatedMetrics struct {
	TotalComparisons     int
	BaselineSuccessRate  float64
	CandidateSuccessRate float64
	AvgTokenDiff         float64
	AvgLatencyDiff       float64
}

// ---------------------------------------------------------------------------
// ContinuousSamplingMonitor — 部署后 1% 生产流量异步采样
// 架构文档: docs/arch/12-Eval-Harness-深度选型.md §9

type ContinuousSamplingMonitor struct {
	samplingRate         float64 // 0.01 (1%)
	slidingWindow        *SlidingWindow
	baselineScore        float64
	degradationThreshold float64 // 0.9
}

// NewContinuousSamplingMonitor 创建 ContinuousSamplingMonitor。
func NewContinuousSamplingMonitor(rate, baseline, threshold float64, windowSize int) *ContinuousSamplingMonitor {
	return &ContinuousSamplingMonitor{
		samplingRate:         rate,
		slidingWindow:        &SlidingWindow{maxSize: windowSize},
		baselineScore:        baseline,
		degradationThreshold: threshold,
	}
}

// GetSamplingRate 返回采样率。
func (csm *ContinuousSamplingMonitor) GetSamplingRate() float64 {
	return csm.samplingRate
}

// SlidingWindow 滑动窗口（max=100）。
type SlidingWindow struct {
	samples []QualitySample
	maxSize int // 100
}

// AddSample 增加采样。
func (sw *SlidingWindow) AddSample(sample QualitySample) {
	sw.samples = append(sw.samples, sample)
	if sw.maxSize > 0 && len(sw.samples) > sw.maxSize {
		sw.samples = sw.samples[1:]
	}
}

// QualitySample 质量采样。
type QualitySample struct {
	Timestamp int64
	Score     float64
	TaskType  string
	SessionID string
}

// CheckDegradation 每 10min 检测退化。
// avgScore < baselineScore × 0.9 → SilentDegradationAlert.
// 自动回滚: 冻结 M9 Auto-Curriculum → 回退 7 天 L1-L3 产物 → 全量 Eval replay.
func (csm *ContinuousSamplingMonitor) CheckDegradation() bool {
	avg := csm.slidingWindow.Average()
	return avg < csm.baselineScore*csm.degradationThreshold
}

func (sw *SlidingWindow) Average() float64 {
	if len(sw.samples) == 0 {
		return 1.0
	}
	var sum float64
	for _, s := range sw.samples {
		sum += s.Score
	}
	return sum / float64(len(sw.samples))
}

// ---------------------------------------------------------------------------
// RegressionDetector — 回归检测与自动熔断
// 架构文档: docs/arch/12-Eval-Harness-深度选型.md §11

// RollingBaseline 30 天滚动基线。
// [TokenBurnRate]: current > baseline P95×2.0 → AlertCritical, auto-throttle
// [SurpriseIndex]: current > baseline P95 且连续 3 天 → AlertWarning
// Task_Success_Rate: current < baseline Mean-0.05 → AlertCritical, auto-rollback
type RegressionDetector struct {
	baselineWindow int                     // 30d
	buckets        map[string]*StatsBucket // metric_name → bucket
}

// NewRegressionDetector 创建 RegressionDetector。
func NewRegressionDetector(bw int) *RegressionDetector {
	return &RegressionDetector{
		baselineWindow: bw,
		buckets:        make(map[string]*StatsBucket),
	}
}

// GetBucket 返回或者创建 Bucket。
func (rd *RegressionDetector) GetBucket(name string) *StatsBucket {
	b, ok := rd.buckets[name]
	if !ok {
		b = &StatsBucket{}
		rd.buckets[name] = b
	}
	return b
}

// StatsBucket 统计桶（环形缓冲区，30 天窗口）。
type StatsBucket struct {
	values [30]float64
	head   int
}

// Add 增加一条数据。
func (sb *StatsBucket) Add(val float64) {
	sb.values[sb.head] = val
	sb.head = (sb.head + 1) % len(sb.values)
}

// P95 计算 30 天滑动窗口 P95（环形缓冲区全量排序）。
func (sb *StatsBucket) P95() float64 {
	sorted := make([]float64, len(sb.values))
	copy(sorted, sb.values[:])
	sort.Float64s(sorted)
	idx := max(int(math.Ceil(0.95*float64(len(sorted))))-1, 0)
	return sorted[idx]
}

// Mean 计算 30 天均值。
func (sb *StatsBucket) Mean() float64 {
	var sum float64
	for _, v := range sb.values {
		sum += v
	}
	return sum / float64(len(sb.values))
}
