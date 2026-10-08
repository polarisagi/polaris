package tts

import (
	"context"
	"time"
)

// Audio 是一次合成的产物：音频字节 + 其 MIME 类型。
// 为什么携带 MIME：HTTP sidecar 可能产出 MP3 而 Sherpa 产出 WAV；接口若假设"一律 WAV"，
// 上层只能硬编码 Content-Type，格式一变就静默错配。
type Audio struct {
	Data []byte
	MIME string
	// Duration 是音频时长；0 表示 Provider 未给出（HTTP sidecar）。
	// 首次启用基准用它计算 RTF = 合成耗时 / 音频时长。
	Duration time.Duration
}

// 常用 MIME 常量。
const (
	MIMEWav = "audio/wav"
	MIMEMP3 = "audio/mpeg"
)

// Provider 是 TTS 引擎的统一抽象接口。
// 两种实现：
//   - *Engine       —— Sherpa-ONNX 本地离线推理（MeloTTS，ADR-0110），无网络依赖，audio/wav
//   - *HTTPProvider —— 外部 HTTP sidecar（CosyVoice 2 / Qwen3-TTS 等 GPU 推理服务，高级可选），取响应 Content-Type
//
// Edge TTS 已于 ADR-0107 删除。Generate 返回的 Audio.MIME 必须与 Data 的真实编码一致。
type Provider interface {
	Generate(ctx context.Context, text string) (Audio, error)
	Close() error
}

// ProviderBox 持有 Provider 接口值，用于 atomic.Pointer[ProviderBox]。
// 规避 atomic.Value 要求"同一具体类型"的限制，使不同实现可以原子替换。
type ProviderBox struct {
	P Provider
}
