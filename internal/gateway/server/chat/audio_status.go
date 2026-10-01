package chat

import (
	"log/slog"
	"sync/atomic"
	"time"
)

// 音频资产状态机的状态取值。
const (
	AudioStateDisabled    = "disabled"    // 被门控/配置禁用，不会尝试准备
	AudioStatePending     = "pending"     // 等待首次准备（或退避等待重试）
	AudioStateDownloading = "downloading" // 正在下载/加载资产
	AudioStateReady       = "ready"       // 可用
	AudioStateFailed      = "failed"      // 最近一次准备失败（后台按退避重试）
)

// AudioAssetStatus 是 STT/TTS 资产当前状态的只读快照，经 /v1/system/capabilities 暴露。
// 为什么需要：此前启动期一次性 fire-and-forget 下载失败后只留一行日志，前端只拿到 503，
// 用户看到"没反应"；有了状态与失败原因，前端才能提示"准备中/失败原因"。
type AudioAssetStatus struct {
	State     string    `json:"state"`
	Detail    string    `json:"detail,omitempty"`
	Error     string    `json:"error,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// AudioStatusTracker 以 atomic 方式存取一份 AudioAssetStatus（并发安全，读写无锁）。
type AudioStatusTracker struct {
	kind string // "stt" | "tts"，仅用于日志
	cur  atomic.Pointer[AudioAssetStatus]
}

// NewAudioStatusTracker 构造 tracker，初始状态为 pending。
func NewAudioStatusTracker(kind string) *AudioStatusTracker {
	t := &AudioStatusTracker{kind: kind}
	t.cur.Store(&AudioAssetStatus{State: AudioStatePending, UpdatedAt: time.Now()})
	return t
}

// Set 切换状态；状态（State）变化时打 slog（HE-1：状态切换可观测）。
// 项目未为此类低频状态注册 Prometheus gauge 的既有惯例（指标集中注册并有基数守卫），
// 不为此新造框架，故只落日志。
func (t *AudioStatusTracker) Set(state, detail, errMsg string) {
	if t == nil {
		return
	}
	prev := t.cur.Load()
	t.cur.Store(&AudioAssetStatus{State: state, Detail: detail, Error: errMsg, UpdatedAt: time.Now()})
	if prev == nil || prev.State != state {
		slog.Info("audio: asset state changed", "kind", t.kind, "from", stateOf(prev), "to", state, "detail", detail, "error", errMsg)
	}
}

// Get 返回当前状态快照。
func (t *AudioStatusTracker) Get() AudioAssetStatus {
	if t == nil {
		return AudioAssetStatus{State: AudioStatePending}
	}
	if p := t.cur.Load(); p != nil {
		return *p
	}
	return AudioAssetStatus{State: AudioStatePending}
}

func stateOf(s *AudioAssetStatus) string {
	if s == nil {
		return ""
	}
	return s.State
}
