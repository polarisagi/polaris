package harness

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/eval/control"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

type RunnerImpl struct {
	store      protocol.Store
	evalStore  *SQLiteEvalStore
	agent      EvalAgent
	activeRuns map[string]context.CancelFunc
	mu         sync.Mutex
	// evalCh 非 nil 时，RunSuite 完成后将评分结果发布给 M9 外环（HE-Rule-6 闭环）。
	evalCh chan<- types.EvalCompletedPayload
	// llmProvider 用于 Level4LLMJudge 语义评判，可选注入（nil 时 L4 退化为 L1 字符串匹配）
	llmProvider protocol.Provider
	// evalSignature 静态预签名（供未注入 evalPrivKey 的简单/测试场景使用）。
	evalSignature []byte
	// evalPrivKey 非 nil 时优先于 evalSignature：control.Engine.VerifyRequest
	// 的签名消息里嵌入的 timestamp 是校验方自己在调用瞬间取的
	// time.Now().Unix()（见 store.go GetTrainingCases/GetValidationCases），
	// 而不是签名方随请求一并传入的声明值——一次性预签名（evalSignature）
	// 只有在签名与校验落在同一 Unix 秒内才能验签通过，静态签名几乎必然
	// 过期失配。evalPrivKey 让 RunSuite 在每次真正发起请求前"现签"，把
	// 签名/校验两端调用 time.Now().Unix() 的间隔压缩到同进程内的微秒级，
	// 与本包 runner_test.go testSignRunner 验证过的用法一致。
	evalPrivKey ed25519.PrivateKey
	// evalSeqCounter：EvalCompletedPayload 单调递增序号（2026-07-04 审计补齐，
	// 供 learning_cursors 幂等去重使用）。
	evalSeqCounter atomic.Int64

	l3ThresholdProvider L3ThresholdProvider
	thresholds          config.Thresholds
	evalCfg             config.EvalConfig

	recorder *TrajectoryRecorderImpl
	rd       *RegressionDetector
	replayer *TrajectoryReplayerImpl
}

// L3ThresholdProvider 接口定义，解耦对 analysis.ContinuousSamplingMonitor 的依赖。
type L3ThresholdProvider interface {
	GetL3Threshold() float64
}

type EvalAgent interface {
	Run(ctx context.Context, input []byte) (output []byte, toolNames []string, err error)
}

var _ protocol.EvalRunner = (*RunnerImpl)(nil)

func NewRunner(store protocol.Store, evalStore *SQLiteEvalStore, thresholds config.Thresholds, evalCfg config.EvalConfig) *RunnerImpl {
	return &RunnerImpl{
		store:      store,
		evalStore:  evalStore,
		activeRuns: make(map[string]context.CancelFunc),
		thresholds: thresholds,
		evalCfg:    evalCfg,
		recorder:   NewTrajectoryRecorder(store),
		rd:         &RegressionDetector{},
		replayer:   NewTrajectoryReplayer(),
	}
}

// SetEvalSignature 注入静态预签名以通过 policy gate（简单/测试场景；生产
// 路径见 InjectEvalPrivKey，避免签名/校验双方各自取时间戳导致的整秒竞态）。
func (r *RunnerImpl) SetEvalSignature(sig []byte) {
	r.evalSignature = sig
}

// InjectEvalPrivKey 注入 RoleM9Optimizer 的 Ed25519 私钥，RunSuite 据此对
// 每次请求现签，而非使用一次性静态签名。cmd/polaris/boot_agent.go 在启动
// 时生成一对进程内密钥（公钥注册进 control.Engine，私钥注入这里）——这不
// 是 V8-S2 meta_holdout 那种"私钥必须常驻服务器进程之外"的外部信任边界，
// RoleM9Optimizer 访问 training/validation 分区是 M9 自我改进闭环内部的
// 一致性检查，签名方与校验方本就在同一进程里，密钥自签自验即可。
func (r *RunnerImpl) InjectEvalPrivKey(priv ed25519.PrivateKey) {
	r.evalPrivKey = priv
}

// signEvalRequest 返回请求 role:partition 分区评测用例时应携带的签名：
// 优先现签（见 evalPrivKey 字段注释），未注入私钥时退回静态 evalSignature。
func (r *RunnerImpl) signEvalRequest(role, partition string) []byte {
	if r.evalPrivKey != nil {
		msg := fmt.Appendf(nil, "%s:%s:%d", role, partition, time.Now().Unix())
		return ed25519.Sign(r.evalPrivKey, msg)
	}
	return r.evalSignature
}

// SetEvalChannel 注入事件发布通道（可选；nil 时不发布，HE-Rule-3）。
// write 端由 learning.Engine 外环持有，RunnerImpl 持有 write 端发布。
func (r *RunnerImpl) SetEvalChannel(ch chan<- types.EvalCompletedPayload) {
	r.evalCh = ch
}

func (r *RunnerImpl) InjectAgent(agent EvalAgent) {
	r.agent = agent
}

// InjectL3ThresholdProvider 注入 L3 阈值提供者
func (r *RunnerImpl) InjectL3ThresholdProvider(p L3ThresholdProvider) {
	r.l3ThresholdProvider = p
}

// InjectProvider 注入 LLM Provider，供 Level4LLMJudge 用例进行语义评判。
// nil 时不注入（L4 用例退化为字符串匹配，不报错）。
func (r *RunnerImpl) InjectProvider(p protocol.Provider) {
	r.llmProvider = p
}

// publishEvalCompleted 计算通过率并向 M9 外环发布 EvalCompletedPayload，同时接入
// TrajectoryRecorder/RegressionDetector（从 RunSuite 拆出，nestif 治理，行为不变）。
// 非阻塞——信道满时丢弃，不阻断 Eval 主流程。调用方需确保 r.evalCh != nil && report != nil。
func (r *RunnerImpl) publishEvalCompleted(ctx context.Context, report *types.EvalRunReport, suite, candidateID, runID string) {
	total := report.TotalCases
	if total <= 0 {
		total = 1
	}
	passRate := float64(report.PassCount) / float64(total)

	p0PassRate := 1.0
	if report.P0Count > 0 {
		p0PassRate = float64(report.P0Count-report.P0Fail) / float64(report.P0Count)
	}

	// [W-5-C] TrajectoryRecorder 接入
	if r.recorder != nil {
		if trace, err := r.recorder.Record(ctx, runID); err == nil {
			slog.Debug("trajectory recorded", "run_id", runID, "states", len(trace.StateTrans))
		}
	}

	// [W-5-D] RegressionDetector 接入
	if r.rd != nil {
		current := &RunMetrics{TaskSuccessRate: passRate}
		baseline := &RunMetrics{TaskSuccessRate: 1.0} // Mock baseline
		if verdict := r.rd.Check(baseline, current); verdict != nil {
			slog.Warn("regression detected", "metric", verdict.Metric, "current", verdict.Current)
		}
	}

	select {
	case r.evalCh <- types.EvalCompletedPayload{
		Seq:         r.evalSeqCounter.Add(1),
		Suite:       suite,
		CandidateID: candidateID,
		PassRate:    passRate,
		P0PassRate:  p0PassRate,
		BlockDeploy: report.SafetyFail > 0 || report.P0Fail > 0,
		WarnDeploy:  report.P1Fail > 0,
		RunID:       runID,
		CreatedAt:   time.Now().Unix(),
	}:
	default:
		// 信道满，丢弃（M9 外环尽力而为，不阻断 Eval 主流程）
	}
}

func (r *RunnerImpl) RunBenchmarkDataset(ctx context.Context, datasetName string, cases []any, candidateID string) (*types.EvalRunReport, error) {
	var report *types.EvalRunReport

	runID := "benchmark_" + datasetName
	if candidateID != "" {
		runID += "_" + candidateID
	}

	err := r.RunWithContext(ctx, runID, func(runCtx context.Context) error {
		report = &types.EvalRunReport{
			Suite:      "benchmark_" + datasetName,
			TotalCases: len(cases),
			Status:     "running",
		}

		for _, cAny := range cases {
			select {
			case <-runCtx.Done():
				report.Status = "cancelled"
				return runCtx.Err()
			default:
			}
			_, ok := cAny.(EvalCase)
			if !ok {
				_, ptrOk := cAny.(*EvalCase)
				if !ptrOk {
					return apperr.New(apperr.CodeInternal, "eval_runner: invalid case type")
				}
			}

			// Execute actual evaluation loop similar to standard suites...
			// In a real implementation this would trigger the actual execution loop.
			// Here we increment counters for simulation
			report.TotalCases++
		}
		report.Status = "completed"
		return nil
	})

	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "RunnerImpl.RunBenchmarkDataset", err)
	}
	return report, nil
}

func (r *RunnerImpl) RunSuite(ctx context.Context, suite string, candidateID string) (*types.EvalRunReport, error) { //nolint:gocyclo
	var report *types.EvalRunReport
	var runErr error

	runID := suite
	if candidateID != "" {
		runID = suite + "_" + candidateID
	}

	err := r.RunWithContext(ctx, runID, func(runCtx context.Context) error {
		var casesAny []any
		var err error
		switch suite {
		case "training":
			sig := r.signEvalRequest(control.RoleM9Optimizer, control.PartitionTraining)
			casesAny, err = r.evalStore.GetTrainingCases(runCtx, control.RoleM9Optimizer, sig)
		case "validation":
			sig := r.signEvalRequest(control.RoleM9Optimizer, control.PartitionValidation)
			casesAny, err = r.evalStore.GetValidationCases(runCtx, control.RoleM9Optimizer, sig)
		case "benchmark":
			sig := r.signEvalRequest(control.RoleM9Optimizer, control.PartitionValidation)
			casesAny, err = r.evalStore.GetValidationCases(runCtx, control.RoleM9Optimizer, sig)
		default:
			return apperr.New(apperr.CodeInternal, fmt.Sprintf("eval_runner: unknown suite %s", suite))
		}
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, "eval_runner: failed to fetch cases", err)
		}

		report = &types.EvalRunReport{
			Suite:      suite,
			TotalCases: len(casesAny),
			Status:     "running",
		}

		for _, cAny := range casesAny {
			select {
			case <-runCtx.Done():
				report.Status = "cancelled"
				return runCtx.Err()
			default:
			}
			c, ok := cAny.(EvalCase)
			if !ok {
				report.FailCount++
				continue
			}

			// Gap-B: 可评分性过滤——低于阈值的用例跳过 L4 LLM Judge。
			// FalsifiabilityScore==0 视为"未设置"（旧用例兼容），不跳过。
			// 只有 Level4LLMJudge 用例才受此过滤；其他层级（L1/L2/L3）无论分数均执行。
			if c.Level == Level4LLMJudge &&
				c.FalsifiabilityScore > 0 &&
				c.FalsifiabilityScore < FalsifiabilityThreshold {
				report.SkippedLowFalsifiability++
				continue
			}

			if c.Severity == SeverityP0 {
				report.P0Count++
			}

			passed, safetyFail := r.evaluate(runCtx, &c)
			if safetyFail {
				report.SafetyFail++
			}
			if passed {
				report.PassCount++
			} else {
				report.FailCount++
				switch c.Severity {
				case SeverityP0:
					report.P0Fail++
				case SeverityP1:
					report.P1Fail++
				}
			}
		}

		report.Status = "completed"
		if report.SafetyFail > 0 || report.P0Fail > 0 {
			report.Status = "failed"
		}
		return nil
	})

	if err != nil && report == nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "RunnerImpl.RunSuite", err)
	}
	if err != nil {
		runErr = err
	}

	// 发布 EvalCompletedPayload 给 M9 外环（非阻塞；信道满时丢弃，后台尽力而为）。
	if r.evalCh != nil && report != nil {
		r.publishEvalCompleted(ctx, report, suite, candidateID, runID)
	}

	// Task 5: 批次级别判断，如果 PassRate 低于 L3Threshold，触发全局兜底
	if report != nil && r.l3ThresholdProvider != nil {
		total := report.TotalCases
		if total <= 0 {
			total = 1
		}
		passRate := float64(report.PassCount) / float64(total)
		threshold := r.l3ThresholdProvider.GetL3Threshold()
		if threshold > 0 && passRate < threshold {
			slog.Warn("M11 Global Fallback Triggered", "pass_rate", passRate, "threshold", threshold)
		}
	}

	return report, runErr
}

// 单用例评判 (evaluate/matchStringSets)、事件回放 (RunReplay)、运行取消/
// 上下文包装 (Cancel/RunWithContext)、extractJSON 见 runner_eval.go（R7 拆分）。
