package tts

import "context"

// Provider 是 TTS 引擎的统一抽象接口。
// 三种实现：
//   - *Engine       —— Sherpa-ONNX 本地离线推理（Kokoro），无网络依赖
//   - *EdgeProvider —— Microsoft Edge TTS WebSocket（免费、无需 API 密钥、中国大陆可用）
//   - *HTTPProvider —— 外部 HTTP sidecar（CosyVoice 2 / Qwen3-TTS 等 GPU 推理服务）
//
// Generate 始终返回标准 WAV 格式字节流（16-bit PCM 单声道）。
type Provider interface {
	Generate(ctx context.Context, text string) ([]byte, error)
	Close() error
}

// ProviderBox 持有 Provider 接口值，用于 atomic.Pointer[ProviderBox]。
// 规避 atomic.Value 要求"同一具体类型"的限制，使三种不同实现可以原子替换。
type ProviderBox struct {
	P Provider
}
