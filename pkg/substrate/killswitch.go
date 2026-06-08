package substrate

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/polarisagi/polaris/pkg/substrate/observability"
)

// PolarisKillswitchStage 保留向后兼容的包级别引用，指向 observability 的全局原子量。
// 新代码使用 observability.GlobalKillswitchStage 直接读写。
var PolarisKillswitchStage = &observability.GlobalKillswitchStage

// KillSwitch — 三阶段紧急停止协议。
// 架构文档: docs/arch/M11-Policy-Safety.md §4

// KillState 紧急停止状态。
type KillState int

const (
	KillNormal   KillState = iota
	KillThrottle           // Stage 1: 降级 Tier1, max_steps=3, 禁止写
	KillPause              // Stage 2: 停止新任务, 保留状态, 通知
	KillFullStop           // Stage 3: 停止所有 goroutine, 写入 kill event
)

// KillSwitch 三阶段 FSM。
// Stage 1 THROTTLE: [TokenBurnRate] > 2x baseline, 连续错误 > 5
// Stage 2 PAUSE: Stage 1 持续 > 10min, 安全违规, 不可逆操作被尝试
// Stage 3 FULLSTOP: Stage 2 未在 15min 内审批, 致命安全违规, 管理员手动
type KillSwitch struct {
	state  KillState
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
	StateChangeCallback func(newState KillState, reason string)

	// TripleCtrlCGuard 状态
	mu          sync.Mutex
	sigintTimes []time.Time // 3s 窗口内的 SIGINT 时间戳

	// dataDir 用于写入 .fullstop 文件（默认 ~/.polarisagi/polaris）
	dataDir string
}

// GetState 返回当前 KillSwitch 阶段的线程安全快照，供 M4/M8/M13 读 gauge 降级响应。
func (ks *KillSwitch) GetState() KillState {
	ks.mu.Lock()
	defer ks.mu.Unlock()
	return ks.state
}

func NewKillSwitch(dataDir string) *KillSwitch {
	return &KillSwitch{
		state:          KillNormal,
		stateEnteredAt: time.Now(),
		dataDir:        dataDir,
	}
}

// IsFullStopFilePresent 检查 dataDir/.fullstop 文件是否存在。
// 供 main.go 在启动时调用：若文件存在，拒绝启动（封印态持久化）。
func IsFullStopFilePresent(dataDir string) bool {
	path := filepath.Join(dataDir, ".fullstop")
	_, err := os.Stat(path)
	return err == nil
}

// CheckAndAct 按优先级检查触发条件并执行状态转移。
func (ks *KillSwitch) CheckAndAct() KillState {
	if ks.shouldFullStop() {
		if ks.state != KillFullStop {
			ks.transition(KillFullStop, "triggered full stop conditions")
		}
		return KillFullStop
	}
	if ks.shouldPause() {
		if ks.state < KillPause {
			ks.transition(KillPause, "triggered pause conditions")
		}
		return KillPause
	}
	if ks.shouldThrottle() {
		if ks.state < KillThrottle {
			ks.transition(KillThrottle, "triggered throttle conditions")
		}
		return KillThrottle
	}
	return ks.state
}

// triggerFullStop 触发 FullStop，调用 transition
func (ks *KillSwitch) triggerFullStop(actor, reason string) error {
	ks.actor = actor
	ks.transition(KillFullStop, reason)
	return nil
}

func (ks *KillSwitch) IsSealed() bool { return ks.state == KillFullStop }

func (ks *KillSwitch) shouldThrottle() bool {
	// 读全局 TokenBurnRate（observability.GlobalTokenBurnRate 是唯一真相源，inference 适配器写入它）
	if observability.GlobalTokenBurnRate.CheckThrottle() >= observability.ThrottleStage1 {
		return true
	}
	return ks.monitors.errorCounter > 5
}

func (ks *KillSwitch) shouldPause() bool {
	if ks.monitors.safetyViolations > 0 || ks.monitors.irreversibleAttempts > 0 {
		return true
	}
	if ks.state == KillThrottle && time.Since(ks.stateEnteredAt) > 10*time.Minute {
		return true
	}
	return false
}

func (ks *KillSwitch) shouldFullStop() bool {
	if ks.monitors.fatalViolations > 0 {
		return true
	}
	if ks.state == KillPause && time.Since(ks.stateEnteredAt) > 15*time.Minute {
		return true
	}
	return false
}

func (ks *KillSwitch) transition(s KillState, reason string) {
	ks.state = s
	ks.reason = reason
	ks.stateEnteredAt = time.Now()

	if s == KillFullStop {
		ks.writeFullStopFile(reason)
	}

	if ks.StateChangeCallback != nil {
		ks.StateChangeCallback(s, reason)
	}
	for _, n := range ks.notifiers {
		_ = n.Send("CRITICAL", "KillSwitch Transition", reason)
	}
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
		_ = os.WriteFile(filepath.Join(dataDir, ".fullstop"), []byte(content), 0o600)
	}
}

func (ks *KillSwitch) ReportError() {
	ks.monitors.errorCounter++
	ks.CheckAndAct()
}

func (ks *KillSwitch) ReportSafetyViolation(fatal bool) {
	if fatal {
		ks.monitors.fatalViolations++
	} else {
		ks.monitors.safetyViolations++
	}
	ks.CheckAndAct()
}

func (ks *KillSwitch) ReportIrreversibleAttempt() {
	ks.monitors.irreversibleAttempts++
	ks.CheckAndAct()
}

func (ks *KillSwitch) ManualFullStop(actor, reason string) {
	ks.actor = actor
	ks.transition(KillFullStop, reason)
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
	defer ks.mu.Unlock()

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

	if len(ks.sigintTimes) >= 3 {
		ks.reason = "triple SIGINT within 3s window"
		ks.actor = "user"
		_ = ks.triggerFullStop("user", "triple SIGINT within 3s window")
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
		// 文件存在 → 触发 FullStop
		ks.reason = "KILLSWITCH file detected at " + killFile
		ks.actor = "operator"
		_ = ks.triggerFullStop("operator", "KILLSWITCH file detected at "+killFile)
	}
}

// IsFullStopped 返回当前是否处于 FullStop 状态。
func (ks *KillSwitch) IsFullStopped() bool {
	return ks.state == KillFullStop
}
