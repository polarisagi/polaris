// founding_anchor.go — 创始行为锚点（V8-S3 缓解机制）。
// 属于 pkg/governance/eval 包，治理评测层。
package eval

import (
	"github.com/polarisagi/polaris/internal/eval/harness"

	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"
)

const (
	// foundingAnchorFile 锚点文件名（相对于 dataDir = ~/.polarisagi/polaris/）。
	foundingAnchorFile = "founding_anchor.json"

	// DriftWarnThreshold 综合漂移评分触发 WARN 告警的阈值。
	DriftWarnThreshold = 0.15
	// DriftFreezeThreshold 综合漂移评分触发 M9 自进化冻结的阈值。
	DriftFreezeThreshold = 0.30

	// MinTasksForAnchor 创建锚点所需的最少任务轨迹数。
	MinTasksForAnchor = 100
)

// FoundingAnchor 创始行为锚点。
// 首次积累 MinTasksForAnchor 条任务轨迹后自动生成，随后永不自动更新。
// 手动更新需 2 位审批者 Ed25519 签名。
type FoundingAnchor struct {
	Version     string              `json:"version"`
	CreatedAt   int64               `json:"created_at"`
	TaskCount   int                 `json:"task_count_at_creation"`
	Fingerprint BehaviorFingerprint `json:"fingerprint"`
	// Signature Ed25519 签名（base64），signing key 与 Holdout/MetaEval 密钥分离。
	// 环境变量: POLARIS_FOUNDING_ANCHOR_PRIVKEY（base64 Ed25519 私钥）
	Signature string `json:"signature"`
}

// BehaviorFingerprint 行为统计指纹（非具体输出，而是分布特征）。
type BehaviorFingerprint struct {
	// ToolBigramDistribution 工具调用 bigram 的归一化频率分布（top 20 pair）。
	ToolBigramDistribution map[string]float64 `json:"tool_bigram_dist"`
	// 输出长度（字节数）三分位数。
	OutputLenP10 int `json:"output_len_p10"`
	OutputLenP50 int `json:"output_len_p50"`
	OutputLenP90 int `json:"output_len_p90"`
	// RefusalRate 拒绝率（KillSwitch 触发 + Cedar forbid）/ 总请求数。
	RefusalRate float64 `json:"refusal_rate"`
	// AvgSurpriseIndex 近 N 条任务的平均 SurpriseIndex。
	AvgSurpriseIndex float64 `json:"avg_surprise_index"`
}

// DriftReport 与创始锚点的偏差报告。
type DriftReport struct {
	ToolBigramJSD     float64  `json:"tool_bigram_jsd"`      // JSD，≥0.15 WARN，≥0.30 FREEZE
	OutputLenDriftPct float64  `json:"output_len_drift_pct"` // P50 变化率（绝对值）
	RefusalRateDelta  float64  `json:"refusal_rate_delta"`   // 拒绝率绝对变化
	SurpriseDriftAbs  float64  `json:"surprise_drift_abs"`   // SurpriseIndex 绝对变化
	OverallDriftScore float64  `json:"overall_drift_score"`  // 综合漂移评分 [0,1]
	TriggeredAlerts   []string `json:"triggered_alerts,omitempty"`
	ShouldFreeze      bool     `json:"should_freeze"`
	ComputedAt        int64    `json:"computed_at"`
}

// ComputeFingerprint 从一批任务轨迹计算行为指纹。
// trajectories 类型来自 pkg/governance/harness.TrajectoryTrace。
func ComputeFingerprint(trajectories []harness.TrajectoryTrace) BehaviorFingerprint {
	if len(trajectories) == 0 {
		return BehaviorFingerprint{}
	}
	bigramCounts := make(map[string]float64)
	var totalBigrams float64
	var outputLens []int

	for _, t := range trajectories {
		calls := t.ToolCalls
		for i := 1; i < len(calls); i++ {
			key := calls[i-1].Name + "→" + calls[i].Name
			bigramCounts[key]++
			totalBigrams++
		}
		for _, lc := range t.LLMCalls {
			if resp, ok := lc.Response["content"].(string); ok {
				outputLens = append(outputLens, len(resp))
			}
		}
	}

	// 归一化 bigram，保留 top 20
	bigramDist := make(map[string]float64)
	if totalBigrams > 0 {
		type kv struct {
			k string
			v float64
		}
		var pairs []kv
		for k, v := range bigramCounts {
			pairs = append(pairs, kv{k, v / totalBigrams})
		}
		for i := 0; i < len(pairs)-1; i++ {
			for j := i + 1; j < len(pairs); j++ {
				if pairs[j].v > pairs[i].v {
					pairs[i], pairs[j] = pairs[j], pairs[i]
				}
			}
		}
		limit := 20
		if len(pairs) < limit {
			limit = len(pairs)
		}
		for _, p := range pairs[:limit] {
			bigramDist[p.k] = p.v
		}
	}

	p10, p50, p90 := computePercentiles(outputLens, 10, 50, 90)
	return BehaviorFingerprint{
		ToolBigramDistribution: bigramDist,
		OutputLenP10:           p10,
		OutputLenP50:           p50,
		OutputLenP90:           p90,
	}
}

// CompareWithAnchor 计算当前指纹与创始锚点的偏差。
func CompareWithAnchor(anchor *FoundingAnchor, current BehaviorFingerprint) DriftReport {
	report := DriftReport{ComputedAt: time.Now().Unix()}

	report.ToolBigramJSD = jensenShannonDivergence(
		anchor.Fingerprint.ToolBigramDistribution,
		current.ToolBigramDistribution,
	)
	if anchor.Fingerprint.OutputLenP50 > 0 {
		report.OutputLenDriftPct = math.Abs(
			float64(current.OutputLenP50-anchor.Fingerprint.OutputLenP50),
		) / float64(anchor.Fingerprint.OutputLenP50)
	}
	report.RefusalRateDelta = current.RefusalRate - anchor.Fingerprint.RefusalRate
	report.SurpriseDriftAbs = math.Abs(current.AvgSurpriseIndex - anchor.Fingerprint.AvgSurpriseIndex)

	// 加权综合评分
	report.OverallDriftScore = 0.4*report.ToolBigramJSD +
		0.3*report.OutputLenDriftPct +
		0.2*math.Abs(report.RefusalRateDelta) +
		0.1*report.SurpriseDriftAbs
	if report.OverallDriftScore > 1.0 {
		report.OverallDriftScore = 1.0
	}

	if report.ToolBigramJSD >= DriftWarnThreshold {
		report.TriggeredAlerts = append(report.TriggeredAlerts, "tool_bigram_jsd")
	}
	if report.OutputLenDriftPct >= 0.20 {
		report.TriggeredAlerts = append(report.TriggeredAlerts, "output_len_drift")
	}
	if math.Abs(report.RefusalRateDelta) >= 0.05 {
		report.TriggeredAlerts = append(report.TriggeredAlerts, "refusal_rate_delta")
	}
	report.ShouldFreeze = report.OverallDriftScore >= DriftFreezeThreshold
	return report
}

// LoadOrCreate 加载现有锚点；不存在且轨迹足够时创建。
// 返回: (anchor, isNewlyCreated, error)
// dataDir: ~/.polarisagi/polaris/
func LoadOrCreate(dataDir string, privKey ed25519.PrivateKey, trajectories []harness.TrajectoryTrace) (*FoundingAnchor, bool, error) {
	path := filepath.Join(dataDir, foundingAnchorFile)

	if data, err := os.ReadFile(path); err == nil {
		var anchor FoundingAnchor
		if err := json.Unmarshal(data, &anchor); err == nil {
			return &anchor, false, nil
		}
	}

	if len(trajectories) < MinTasksForAnchor {
		return nil, false, apperr.New(apperr.CodeInternal, fmt.Sprintf(
			"founding anchor: need %d trajectories, have %d",
			MinTasksForAnchor, len(trajectories),
		))
	}

	fp := ComputeFingerprint(trajectories)
	anchor := &FoundingAnchor{
		Version:     "1.0",
		CreatedAt:   time.Now().Unix(),
		TaskCount:   len(trajectories),
		Fingerprint: fp,
	}
	if privKey != nil {
		payload, _ := json.Marshal(anchor.Fingerprint)
		sig := ed25519.Sign(privKey, payload)
		anchor.Signature = base64.StdEncoding.EncodeToString(sig)
	}

	data, err := json.MarshalIndent(anchor, "", "  ")
	if err != nil {
		return nil, false, fmt.Errorf("LoadOrCreate: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return nil, false, fmt.Errorf("LoadOrCreate: %w", err)
	}
	return anchor, true, nil
}

// VerifySignature 校验锚点签名。pubKey 为 nil 时开发模式放行。
func VerifySignature(anchor *FoundingAnchor, pubKey ed25519.PublicKey) bool {
	if anchor.Signature == "" || pubKey == nil {
		return true
	}
	payload, err := json.Marshal(anchor.Fingerprint)
	if err != nil {
		return false
	}
	sig, err := base64.StdEncoding.DecodeString(anchor.Signature)
	if err != nil {
		return false
	}
	return ed25519.Verify(pubKey, payload, sig)
}

// DriftMonitor 监控和存储全局漂移评分（供 metrics.go RegisterCallback 读取）。
type DriftMonitor struct {
	score atomic.Value
}

func NewDriftMonitor() *DriftMonitor {
	m := &DriftMonitor{}
	m.score.Store(0.0)
	return m
}

// SetScore 更新漂移评分。
func (m *DriftMonitor) SetScore(score float64) { m.score.Store(score) }

// GetScore 读取最新漂移评分。
func (m *DriftMonitor) GetScore() float64 {
	if v := m.score.Load(); v != nil {
		return v.(float64)
	}
	return 0.0
}

// ─── 内部工具函数 ────────────────────────────────────────────────────────────

func jensenShannonDivergence(p, q map[string]float64) float64 {
	keys := make(map[string]struct{})
	for k := range p {
		keys[k] = struct{}{}
	}
	for k := range q {
		keys[k] = struct{}{}
	}
	if len(keys) == 0 {
		return 0
	}
	var jsd float64
	for k := range keys {
		pi, qi := p[k], q[k]
		m := (pi + qi) / 2
		if m > 0 {
			if pi > 0 {
				jsd += pi * math.Log2(pi/m)
			}
			if qi > 0 {
				jsd += qi * math.Log2(qi/m)
			}
		}
	}
	jsd /= 2
	if jsd > 1.0 {
		jsd = 1.0
	}
	return jsd
}

func computePercentiles(data []int, pcts ...int) (int, int, int) {
	if len(data) == 0 {
		return 0, 0, 0
	}
	sorted := make([]int, len(data))
	copy(sorted, data)
	for i := 0; i < len(sorted)-1; i++ {
		for j := i + 1; j < len(sorted); j++ {
			if sorted[j] < sorted[i] {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}
	get := func(p int) int {
		idx := (p * len(sorted)) / 100
		if idx >= len(sorted) {
			idx = len(sorted) - 1
		}
		return sorted[idx]
	}
	if len(pcts) < 3 {
		return 0, 0, 0
	}
	return get(pcts[0]), get(pcts[1]), get(pcts[2])
}

// GetCreatedAt 返回创建时间（用于实现 swarm 包的接口，解耦依赖）。
func (a *FoundingAnchor) GetCreatedAt() int64 { return a.CreatedAt }

// GetTaskCount 返回创建时的任务数（用于实现 swarm 包的接口，解耦依赖）。
func (a *FoundingAnchor) GetTaskCount() int { return a.TaskCount }
