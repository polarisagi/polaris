package security

import (
	"github.com/polarisagi/polaris/internal/observability/trace"

	"github.com/polarisagi/polaris/internal/observability/metrics"

	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/pkg/types"
)

// KillSwitch — 三阶段紧急停止协议。
// 架构文档: docs/arch/M11-Policy-Safety.md §4

// KillSwitch 三阶段 FSM（规范：state.yaml kill_switch.stages）。
// Stage 1 THROTTLE: [TokenBurnRate] > 2x baseline, 连续错误 > 5；条件消失后自动回落 Normal
// Stage 2 PAUSE: Stage 1 持续 > 10min, 安全违规, 不可逆操作被尝试；仅人工恢复
// Stage 3 FULLSTOP: Stage 2 未在 15min 内审批, 致命安全违规, 管理员手动；仅 unseal
//
// 燃烧率只能触发 Stage 1：Stage 3 写 .fullstop 后进程永久拒绝启动，而燃烧率是成本信号、
// 不是被攻陷信号（2026-10-28 复核，实证见 state.yaml token_burn_rate）。
type KillSwitch struct {
	state  types.KillState
	reason string
	actor  string

	stateEnteredAt time.Time

	// now 可注入时钟，使 10min/15min 时间条件与错误衰减可确定性测试。
	now func() time.Time
	// lastErrorAt 最近一次 ReportError 时刻，用于"连续错误"的安静期清零。
	lastErrorAt time.Time

	monitors struct {
		errorCounter         int
		safetyViolations     int
		fatalViolations      int
		irreversibleAttempts int
	}

	notifiers []Notifier

	// StateChangeCallback 在 KillSwitch 状态变迁时回调，供 M3 Observability 更新
	// polaris_killswitch_stage Prometheus Gauge。非 nil 即启用。
	StateChangeCallback func(newState types.KillState, reason string)

	// TripleCtrlCGuard 状态
	mu          sync.Mutex
	sigintTimes []time.Time // 3s 窗口内的 SIGINT 时间戳

	// dataDir 用于写入 .fullstop 文件（默认 ~/.polaris）
	dataDir string
	tbr     *metrics.TokenBurnRate

	recoveryCallback func(ctx context.Context)
}

// GetState 返回当前 KillSwitch 阶段的线程安全快照，供 M4/M8/M13 读 gauge 降级响应。
func (ks *KillSwitch) GetState() types.KillState {
	ks.mu.Lock()
	defer ks.mu.Unlock()
	return ks.state
}

func NewKillSwitch(dataDir string, tbr *metrics.TokenBurnRate) *KillSwitch {
	return &KillSwitch{
		state:          types.KillNormal,
		reason:         "System OK",
		actor:          "system",
		stateEnteredAt: time.Now(),
		now:            time.Now,
		dataDir:        dataDir,
		tbr:            tbr,
	}
}

// IsFullStopFilePresent 检查 dataDir/.fullstop 文件是否存在。
// 供 main.go 在启动时调用：若文件存在，拒绝启动（封印态持久化）。
func IsFullStopFilePresent(dataDir string) bool {
	path := filepath.Join(dataDir, ".fullstop")
	_, err := os.Stat(path)
	return err == nil
}

// CheckAndAct 按优先级检查触发条件并执行状态转移（持锁）。
func (ks *KillSwitch) CheckAndAct() types.KillState {
	ks.mu.Lock()
	state, needsWrite, reason := ks.checkAndActLocked()
	ks.mu.Unlock()
	if needsWrite {
		ks.writeFullStopFile(reason)
	}
	return state
}

// errorQuietReset "连续错误"的安静期：此时长内无新错误则计数清零。
// 没有成功信号可用来中断连续性，用时间衰减近似，否则累计 6 个错误后 Throttle 永远无法自动回落。
const errorQuietReset = 2 * time.Minute

// checkAndActLocked 在 mu 已持有时执行状态检查与转移（内部使用）。
// 升级随条件即时发生；仅 Throttle 在条件消失后自动回落（state.yaml Stage1 recovery: auto），
// Pause/FullStop 必须经 ManualRecover/Unseal，绝不自动降级。
func (ks *KillSwitch) checkAndActLocked() (types.KillState, bool, string) {
	if ks.monitors.errorCounter > 0 && ks.now().Sub(ks.lastErrorAt) > errorQuietReset {
		ks.monitors.errorCounter = 0
	}

	target := types.KillNormal
	reason := ""
	switch {
	case ks.shouldFullStopLocked():
		target, reason = types.KillFullStop, "triggered full stop conditions"
	case ks.shouldPauseLocked():
		target, reason = types.KillPause, "triggered pause conditions"
	case ks.shouldThrottleLocked():
		target, reason = types.KillThrottle, "triggered throttle conditions"
	}

	switch {
	case target > ks.state:
		burn := ks.tbr != nil && ks.tbr.OverThrottleLine()
		needs := ks.transitionLocked(target, reason)
		if target == types.KillThrottle && burn {
			trace.IncrBurnThrottle()
		}
		if needs {
			return target, true, reason
		}
		return target, false, ""
	case ks.state == types.KillThrottle && target == types.KillNormal:
		ks.transitionLocked(types.KillNormal, "throttle conditions cleared, auto recovery")
	}
	return ks.state, false, ""
}

// triggerFullStop 触发 FullStop，调用 transitionLocked（须在 mu 持有时调用）。
func (ks *KillSwitch) triggerFullStop(actor, reason string) (bool, string) {
	ks.actor = actor
	needs := ks.transitionLocked(types.KillFullStop, reason)
	return needs, reason
}

// IsSealed 返回当前是否处于封印（FullStop）状态（持锁读）。
func (ks *KillSwitch) IsSealed() bool {
	ks.mu.Lock()
	defer ks.mu.Unlock()
	return ks.state == types.KillFullStop
}

// Allowed 返回当前系统是否允许启动新的 Agent 执行 / 后台任务。
// 实现 agent.KillSwitchGate / automation.BackgroundGate 等消费端接口（HE-3）。
// Pause/FullStop 阶段返回 false（拒绝新工作）；Normal/Throttle 阶段仍放行
// （Throttle 仅表示降级，由各消费方自行限流，语义见 types.KillState 定义）。
func (ks *KillSwitch) Allowed() bool {
	ks.mu.Lock()
	defer ks.mu.Unlock()
	return ks.state < types.KillPause
}

func (ks *KillSwitch) shouldThrottleLocked() bool {
	// 读 TokenBurnRate（metrics.TokenBurnRate 是唯一真相源，inference 适配器写入它）
	if ks.tbr != nil && ks.tbr.OverThrottleLine() {
		return true
	}
	return ks.monitors.errorCounter > 5
}

// shouldPauseLocked 须在 mu 持有时调用。
func (ks *KillSwitch) shouldPauseLocked() bool {
	if ks.monitors.safetyViolations > 0 || ks.monitors.irreversibleAttempts > 0 {
		return true
	}
	// 须 Stage1 条件仍然成立才算"持续"；条件已消失的 Throttle 应回落而不是升级。
	if ks.state == types.KillThrottle && ks.shouldThrottleLocked() && ks.now().Sub(ks.stateEnteredAt) > 10*time.Minute {
		return true
	}
	return false
}

// shouldFullStopLocked 须在 mu 持有时调用。
func (ks *KillSwitch) shouldFullStopLocked() bool {
	if ks.monitors.fatalViolations > 0 {
		return true
	}
	if ks.state == types.KillPause && ks.now().Sub(ks.stateEnteredAt) > 15*time.Minute {
		return true
	}
	return false
}

// transitionLocked 执行状态转移，须在 mu 持有时调用。
// 返回 needsFullStop(bool)，若为 true 调用方须在解锁后调用 writeFullStopFile。
func (ks *KillSwitch) transitionLocked(s types.KillState, reason string) bool {
	ks.state = s
	ks.reason = reason
	ks.stateEnteredAt = ks.now()

	needsFullStop := s == types.KillFullStop

	if ks.StateChangeCallback != nil {
		ks.StateChangeCallback(s, reason)
	}
	for _, n := range ks.notifiers {
		// 通知失败不阻断状态迁移（熔断本身必须先生效），但必须留痕：
		// KillSwitch 迁移是最高等级的运维事件，"运维没收到告警"本身就是事故。
		if err := n.Send("CRITICAL", "KillSwitch Transition", reason); err != nil {
			slog.Error("killswitch: transition notification failed, operators may be unaware",
				"stage", s, "reason", reason, "err", err)
		}
	}

	return needsFullStop
}

func (ks *KillSwitch) writeFullStopFile(reason string) {
	dataDir := ks.dataDir
	if dataDir == "" {
		// 未指定时默认使用全局规范数据目录
		dataDir, _ = config.DefaultDataDir()
	}
	if dataDir != "" {
		// 目录创建失败必须 fail-closed，与下方 WriteFile 失败同等对待：
		// .fullstop 是 KillSwitch 跨重启的**唯一**持久化凭证，写不成功意味着
		// 进程重启后会以为从未熔断过、直接恢复服务——这正是熔断要防的事。
		// 此前这里静默丢弃 MkdirAll 错误，随后 WriteFile 必然也失败并 panic，
		// 结果一致但 panic 信息指向 WriteFile，掩盖了真实根因（目录不可创建）。
		if err := os.MkdirAll(dataDir, 0o700); err != nil {
			panic(fmt.Sprintf("killswitch: failed to create data dir %q for .fullstop marker (fail-closed): %v", dataDir, err))
		}
		actor := ks.actor
		if actor == "" {
			actor = "system"
		}
		content := fmt.Sprintf("{\"timestamp\":%d,\"reason\":%q,\"actor\":%q}\n",
			time.Now().Unix(), reason, actor)
		if err := os.WriteFile(filepath.Join(dataDir, config.FullStopFileName), []byte(content), 0o600); err != nil {
			panic(fmt.Sprintf("killswitch: failed to write .fullstop file (fail-closed): %v", err))
		}
	}
}

// ReportError 线程安全地递增错误计数并检查状态转移。
func (ks *KillSwitch) ReportError() {
	ks.mu.Lock()
	ks.monitors.errorCounter++
	ks.lastErrorAt = ks.now()
	_, needsWrite, reason := ks.checkAndActLocked()
	ks.mu.Unlock()
	if needsWrite {
		ks.writeFullStopFile(reason)
	}
}

// ReportSafetyViolation 线程安全地记录安全违规并检查状态转移。
func (ks *KillSwitch) ReportSafetyViolation(fatal bool) {
	ks.mu.Lock()
	if fatal {
		ks.monitors.fatalViolations++
	} else {
		ks.monitors.safetyViolations++
	}
	_, needsWrite, reason := ks.checkAndActLocked()
	ks.mu.Unlock()
	if needsWrite {
		ks.writeFullStopFile(reason)
	}
}

// ReportIrreversibleAttempt 线程安全地记录不可逆操作尝试并检查状态转移。
func (ks *KillSwitch) ReportIrreversibleAttempt() {
	ks.mu.Lock()
	ks.monitors.irreversibleAttempts++
	_, needsWrite, reason := ks.checkAndActLocked()
	ks.mu.Unlock()
	if needsWrite {
		ks.writeFullStopFile(reason)
	}
}

// ManualFullStop 线程安全地手动触发 FullStop。
func (ks *KillSwitch) ManualFullStop(actor, reason string) {
	ks.mu.Lock()
	ks.actor = actor
	needs := ks.transitionLocked(types.KillFullStop, reason)
	ks.mu.Unlock()
	if needs {
		ks.writeFullStopFile(reason)
	}
}

// Notifier 通知接口（Slack/Email/PagerDuty）。
type Notifier interface {
	Send(level string, title string, body string) error
}

// ─── 物理触发路径 ─────────────────────────────────────────────────────────────

// OnSIGINT 实现 TripleCtrlCGuard：在 3s 滑动窗口内计数 SIGINT，
// 达到 3 次立即触发 FullStop。
// 调用方：main.go 的 signal.Notify 处理器。
func (ks *KillSwitch) OnSIGINT() {
	ks.mu.Lock()

	now := time.Now()
	// 清除 3s 窗口外的旧记录
	var recent []time.Time
	for _, t := range ks.sigintTimes {
		if now.Sub(t) <= 3*time.Second {
			recent = append(recent, t)
		}
	}
	recent = append(recent, now)
	ks.sigintTimes = recent

	var needsWrite bool
	var reason string
	if len(ks.sigintTimes) >= 3 {
		ks.reason = "triple SIGINT within 3s window"
		ks.actor = "user"
		needsWrite, reason = ks.triggerFullStop("user", "triple SIGINT within 3s window")
	}

	ks.mu.Unlock()

	if needsWrite {
		ks.writeFullStopFile(reason)
	}
}

// CheckKILLSWITCHFile 轮询检查 KILLSWITCH 文件是否存在。
// 如果存在则立即触发 FullStop。
// 调用方：在 goroutine 中以 500ms 间隔定期调用，
// 或替换为 fsnotify watcher（Tier 1+ 优化）。
func (ks *KillSwitch) CheckKILLSWITCHFile() {
	dataDir := ks.dataDir
	if dataDir == "" {
		dataDir, _ = config.DefaultDataDir()
	}
	if dataDir == "" {
		return
	}
	killFile := filepath.Join(dataDir, config.KillSwitchFileName)
	if _, err := os.Stat(killFile); err == nil {
		// 文件存在 → 触发 FullStop（持锁）。已处于 FullStop 时不重复迁移：
		// 轮询周期 500ms，transitionLocked 每次都会推送 CRITICAL 通知。
		ks.mu.Lock()
		if ks.state == types.KillFullStop {
			ks.mu.Unlock()
			return
		}
		ks.actor = "operator"
		needsWrite, reason := ks.triggerFullStop("operator", "KILLSWITCH file detected at "+killFile)
		ks.mu.Unlock()
		if needsWrite {
			ks.writeFullStopFile(reason)
		}
	}
}
