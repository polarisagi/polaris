package chat

import (
	"log/slog"
	"sync/atomic"
	"time"
)

// 音频资产状态机的状态取值。
const (
	AudioStateDisabled     = "disabled"      // 被配置禁用，不会尝试准备
	AudioStatePending      = "pending"       // 初始占位：状态机尚未被后端接线汇报过
	AudioStateNotInstalled = "not_installed" // 资产未下载；首次使用时由前端触发 install（ADR-0107）
	AudioStateUnsupported  = "unsupported"   // 本机硬件画像低于最低配置，或首次基准判定过慢
	AudioStateDownloading  = "downloading"   // 正在下载并校验资产
	AudioStateLoading      = "loading"       // 正在把引擎加载进内存（含首次基准）
	AudioStateReady        = "ready"         // 资产就绪可用（引擎可能尚未加载或已空闲卸载，见 Loaded）
	AudioStateFailed       = "failed"        // 最近一次下载/加载失败（再次 install 或再次请求即重试）
)

// AudioProgress 是下载进度；BytesTotal 为 0 表示总量未知。
type AudioProgress struct {
	BytesDone  int64 `json:"bytes_done"`
	BytesTotal int64 `json:"bytes_total"`
}

// AudioAssetStatus 是 STT/TTS 资产当前状态的只读快照，经 /v1/system/capabilities 暴露。
// 为什么需要：此前启动期一次性 fire-and-forget 下载失败后只留一行日志，前端只拿到 503，
// 用户看到"没反应"；有了状态与失败原因，前端才能提示"准备中/失败原因"。
type AudioAssetStatus struct {
	State  string `json:"state"`
	Detail string `json:"detail,omitempty"`
	Error  string `json:"error,omitempty"`
	// Reason 是 State=unsupported 时的机器可读原因码（insufficient_ram / insufficient_cores / tts_ram_tier(B 档仅 TTS) /
	// unsupported_platform / too_slow），前端据此选文案。
	Reason string `json:"reason,omitempty"`
	// Model 是 TTS 当前模型名："melo" | "none"（本机不支持或基准过慢）；STT 状态不填（ADR-0110）。
	Model string `json:"model,omitempty"`
	// Origin 标识发起方："auto"|"user"，仅 downloading|loading 时填写。
	Origin string `json:"origin,omitempty"`
	// NextRetryAt 处于退避等待时填写。
	NextRetryAt *time.Time `json:"next_retry_at,omitempty"`
	// InstallSizeBytes 是 State=not_installed 时还需下载的总字节数（供"首次使用需下载约 X MB"确认框）。
	InstallSizeBytes int64 `json:"install_size_bytes,omitempty"`
	// Loaded 表示引擎当前是否驻留内存（State=ready 时有意义；空闲会被卸载）。
	Loaded bool `json:"loaded"`
	// Progress 仅在 State=downloading 且已有字节进度时出现。
	Progress  *AudioProgress `json:"progress,omitempty"`
	UpdatedAt time.Time      `json:"updated_at"`
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
	t.Replace(AudioAssetStatus{State: state, Detail: detail, Error: errMsg})
}

// Replace 整体替换状态快照（UpdatedAt 由本方法填写）。下载进度每秒数次地调用它，
// 所以只在 State 变化时打日志，同状态内的进度刷新不产生日志。
func (t *AudioStatusTracker) Replace(st AudioAssetStatus) {
	if t == nil {
		return
	}
	st.UpdatedAt = time.Now()
	prev := t.cur.Swap(&st)
	if prev == nil || prev.State != st.State {
		slog.Info("audio: asset state changed", "kind", t.kind, "from", stateOf(prev), "to", st.State,
			"detail", st.Detail, "reason", st.Reason, "error", st.Error)
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
