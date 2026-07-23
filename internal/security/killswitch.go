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

	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// KillSwitch — 三阶段紧急停止协议。
// 架构文档: docs/arch/M11-Policy-Safety.md §4

// KillSwitch 三阶段 FSM。
// Stage 1 THROTTLE: [TokenBurnRate] > 2x baseline, 连续错误 > 5
// Stage 2 PAUSE: Stage 1 持续 > 10min, 安全违规, 不可逆操作被尝试
// Stage 3 FULLSTOP: Stage 2 未在 15min 内审批, 致命安全违规, 管理员手动
type KillSwitch struct {
	state  types.KillState
	reason string
	actor  string

	stateEnteredAt time.Time

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

	// dataDir 用于写入 .fullstop 文件（默认 ~/.polarisagi/polaris）
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

// checkAndActLocked 在 mu 已持有时执行状态检查与转移（内部使用）。
func (ks *KillSwitch) checkAndActLocked() (types.KillState, bool, string) {
	if ks.shouldFullStopLocked() {
		if ks.state != types.KillFullStop {
			needs := ks.transitionLocked(types.KillFullStop, "triggered full stop conditions")
			return types.KillFullStop, needs, "triggered full stop conditions"
		}
		return types.KillFullStop, false, ""
	}
	if ks.shouldPauseLocked() {
		if ks.state < types.KillPause {
			ks.transitionLocked(types.KillPause, "triggered pause conditions")
		}
		return types.KillPause, false, ""
	}
	if ks.shouldThrottleLocked() {
		if ks.state < types.KillThrottle {
			ks.transitionLocked(types.KillThrottle, "triggered throttle conditions")
		}
		return types.KillThrottle, false, ""
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
	if ks.tbr != nil && ks.tbr.CheckThrottle() >= metrics.ThrottleStage1 {
		return true
	}
	return ks.monitors.errorCounter > 5
}

// shouldPauseLocked 须在 mu 持有时调用。
func (ks *KillSwitch) shouldPauseLocked() bool {
	if ks.monitors.safetyViolations > 0 || ks.monitors.irreversibleAttempts > 0 {
		return true
	}
	if ks.state == types.KillThrottle && time.Since(ks.stateEnteredAt) > 10*time.Minute {
		return true
	}
	return false
}

// shouldFullStopLocked 须在 mu 持有时调用。
func (ks *KillSwitch) shouldFullStopLocked() bool {
	if ks.monitors.fatalViolations > 0 {
		return true
	}
	if ks.state == types.KillPause && time.Since(ks.stateEnteredAt) > 15*time.Minute {
		return true
	}
	if ks.tbr != nil && ks.tbr.CheckThrottle() >= metrics.ThrottleStage3 {
		trace.IncrBurnStage3()
		return true
	}
	return false
}

// transitionLocked 执行状态转移，须在 mu 持有时调用。
// 返回 needsFullStop(bool)，若为 true 调用方须在解锁后调用 writeFullStopFile。
func (ks *KillSwitch) transitionLocked(s types.KillState, reason string) bool {
	ks.state = s
	ks.reason = reason
	ks.stateEnteredAt = time.Now()

	needsFullStop := s == types.KillFullStop

	if ks.StateChangeCallback != nil {
		ks.StateChangeCallback(s, reason)
	}
	for _, n := range ks.notifiers {
		_ = n.Send("CRITICAL", "KillSwitch Transition", reason)
	}
	
	return needsFullStop
}

func (ks *KillSwitch) writeFullStopFile(reason string) {
	dataDir := ks.dataDir
	if dataDir == "" {
		if home, err := os.UserHomeDir(); err == nil {
			dataDir = filepath.Join(home, ".polarisagi/polaris")
		}
	}
	if dataDir != "" {
		_ = os.MkdirAll(dataDir, 0o700)
		actor := ks.actor
		if actor == "" {
			actor = "system"
		}
		content := fmt.Sprintf("{\"timestamp\":%d,\"reason\":%q,\"actor\":%q}\n",
			time.Now().Unix(), reason, actor)
		if err := os.WriteFile(filepath.Join(dataDir, ".fullstop"), []byte(content), 0o600); err != nil {
			panic(fmt.Sprintf("killswitch: failed to write .fullstop file (fail-closed): %v", err))
		}
	}
}

// ReportError 线程安全地递增错误计数并检查状态转移。
func (ks *KillSwitch) ReportError() {
	ks.mu.Lock()
	ks.monitors.errorCounter++
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

// CheckKILLSWITCHFile 轮询检查 ~/.polarisagi/polaris/KILLSWITCH 文件是否存在。
// 如果存在则立即触发 FullStop。
// 调用方：在 goroutine 中以 500ms 间隔定期调用，
// 或替换为 fsnotify watcher（Tier 1+ 优化）。
func (ks *KillSwitch) CheckKILLSWITCHFile() {
	dataDir := ks.dataDir
	if dataDir == "" {
		if home, err := os.UserHomeDir(); err == nil {
			dataDir = filepath.Join(home, ".polarisagi/polaris")
		}
	}
	if dataDir == "" {
		return
	}
	killFile := filepath.Join(dataDir, "KILLSWITCH")
	if _, err := os.Stat(killFile); err == nil {
		// 文件存在 → 触发 FullStop（持锁）
		ks.mu.Lock()
		ks.actor = "operator"
		needsWrite, reason := ks.triggerFullStop("operator", "KILLSWITCH file detected at "+killFile)
		ks.mu.Unlock()
		if needsWrite {
			ks.writeFullStopFile(reason)
		}
	}
}

// IsFullStopped 返回当前是否处于 FullStop 状态（持锁读）。
func (ks *KillSwitch) IsFullStopped() bool {
	ks.mu.Lock()
	defer ks.mu.Unlock()
	return ks.state == types.KillFullStop
}

// OnRecovery 注册恢复回调
func (ks *KillSwitch) OnRecovery(cb func(ctx context.Context)) {
	ks.mu.Lock()
	defer ks.mu.Unlock()
	ks.recoveryCallback = cb
}

// removeFullStopFile 删除全停文件。
func (ks *KillSwitch) removeFullStopFile() error {
	dataDir := ks.dataDir
	if dataDir == "" {
		if home, err := os.UserHomeDir(); err == nil {
			dataDir = filepath.Join(home, ".polarisagi/polaris")
		}
	}
	if dataDir == "" {
		return nil
	}
	fullStopFile := filepath.Join(dataDir, ".fullstop")
	if err := os.Remove(fullStopFile); err != nil && !os.IsNotExist(err) {
		return apperr.Wrap(apperr.CodeInternal, "failed to remove fullstop file", err)
	}
	return nil
}

// ManualRecover 线程安全地手动触发恢复（解除封印）。
func (ks *KillSwitch) ManualRecover(ctx context.Context, actor, reason string) error {
	ks.mu.Lock()
	wasSealed := ks.state == types.KillFullStop

	if wasSealed {
		if err := ks.removeFullStopFile(); err != nil {
			ks.mu.Unlock()
			slog.Error("killswitch: failed to remove .fullstop file", "err", err)
			return apperr.Wrap(apperr.CodeInternal, "killswitch: failed to remove .fullstop file", err)
		}
	}

	ks.actor = actor
	ks.monitors.errorCounter = 0
	ks.monitors.safetyViolations = 0
	ks.monitors.fatalViolations = 0
	ks.monitors.irreversibleAttempts = 0
	ks.transitionLocked(types.KillNormal, reason)
	cb := ks.recoveryCallback
	ks.mu.Unlock()

	if wasSealed && cb != nil {
		cb(ctx)
	}
	return nil
}

// Unseal 是最高权限的管理端点调用的解封方法，等价于 ManualRecover
func (ks *KillSwitch) Unseal(ctx context.Context, actor, reason string) error {
	return ks.ManualRecover(ctx, actor, reason)
}
