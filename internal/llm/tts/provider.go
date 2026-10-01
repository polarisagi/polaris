package tts

import "context"

// Audio 是一次合成的产物：音频字节 + 其 MIME 类型。
// 为什么携带 MIME：Edge 免费端点只产出 MP3，而 Sherpa 产出 WAV；接口若假设"一律 WAV"，
// 上层只能硬编码 Content-Type，格式一变就静默错配。
type Audio struct {
	Data []byte
	MIME string
}

// 常用 MIME 常量。
const (
	MIMEWav = "audio/wav"
	MIMEMP3 = "audio/mpeg"
)

// Provider 是 TTS 引擎的统一抽象接口。
// 三种实现：
//   - *Engine       —— Sherpa-ONNX 本地离线推理（Kokoro），无网络依赖，audio/wav
//   - *EdgeProvider —— Microsoft Edge TTS WebSocket（免费、无需 API 密钥、中国大陆可用），audio/mpeg
//   - *HTTPProvider —— 外部 HTTP sidecar（CosyVoice 2 / Qwen3-TTS 等 GPU 推理服务），取响应 Content-Type
//
// Generate 返回的 Audio.MIME 必须与 Data 的真实编码一致。
type Provider interface {
	Generate(ctx context.Context, text string) (Audio, error)
	Close() error
}

// ProviderBox 持有 Provider 接口值，用于 atomic.Pointer[ProviderBox]。
// 规避 atomic.Value 要求"同一具体类型"的限制，使三种不同实现可以原子替换。
type ProviderBox struct {
	P Provider
}
