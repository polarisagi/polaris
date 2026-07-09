package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/gateway/server"
	"github.com/polarisagi/polaris/internal/gateway/server/chat"
	"github.com/polarisagi/polaris/internal/llm/stt"
	"github.com/polarisagi/polaris/internal/llm/tts"
	"github.com/polarisagi/polaris/internal/observability/probe"
)

// InitSTTEngine 按 FeatureGate 门控初始化 STT 引擎。
// 必须在 NewServer 之后、Start 之前调用（或与 Start 并发，mock 引擎已就绪）。
// 流程：
//  1. 立即注入 mock 引擎（保证 /v1/audio/transcriptions 不返回 503）
//  2. 若门控禁用，仅打 Info 日志后返回
//  3. 否则在后台 goroutine：EnsureAssets → LoadLibrary → NewEngine → 替换为真实引擎
func initSTTEngine(ctx context.Context, s *server.Server, dataDir string, gate *probe.FeatureGate, httpClient *http.Client, sttConfig config.STTConfig) {
	sttDir := filepath.Join(dataDir, "models", "sensevoice")

	// 立即设置 mock 引擎，保证接口可用
	// if mockEngine...

	// 门控检查：FeatureLocalSTT 是最低档（int8），未开启则无法运行 STT
	if gate != nil && gate.State(probe.FeatureLocalSTT) == probe.FeatureDisabled {
		slog.Info("stt: FeatureLocalSTT disabled by FeatureGate (need ≥512MB free), using mock engine")
		return
	}

	// 按 FeatureHQSTT 自动选择模型档位：
	//   HQ 门控开启（≥1GB free）→ float32 SenseVoice（精度优先）
	//   HQ 门控未开启              → int8 SenseVoice（速度/体积优先）
	useHQ := gate != nil && gate.State(probe.FeatureHQSTT) != probe.FeatureDisabled
	modelURL := sttConfig.SenseVoiceModelURL // 默认 float32（HQ）
	if !useHQ {
		if sttConfig.SenseVoiceModelURLStd != "" {
			modelURL = sttConfig.SenseVoiceModelURLStd // int8 标准档
		}
		// SenseVoiceModelURLStd 为空（旧配置）则回退到 SenseVoiceModelURL
	}

	// 异步下载 + 重载：不阻塞启动路径
	//nolint:bare-goroutine // 历史代码暂留，需结合上下文梳理 ctx 传递链路，后续重构替换
	go func() {
		if err := stt.EnsureAssets(ctx, sttDir, httpClient, sttConfig.SherpaVersion, modelURL, sttConfig.PunctModelURL); err != nil {
			slog.Warn("stt: asset download failed, keeping mock engine", "err", err)
			return
		}

		libPath := filepath.Join(sttDir, stt.LibName())
		if err := stt.LoadLibrary(libPath); err != nil {
			slog.Warn("stt: library load failed after download, keeping mock engine", "err", err)
			return
		}

		modelDir := stt.ModelDir(sttDir)
		engine, err := stt.NewEngine(modelDir, stt.PunctModelDir(sttDir))
		if err != nil {
			slog.Warn("stt: engine init failed", "err", err)
			return
		}
		s.SetSTTProvider(&sttAdapter{inner: engine})
		slog.Info("stt: real engine active (sherpa-onnx SenseVoice)",
			"model_dir", modelDir,
			"hq", useHQ,
			"model_url", modelURL,
		)
	}()
}

// InitTTSEngine 初始化 TTS Provider 并注入 ChatHandler。
//
// 三条路径由 ttsConfig.Provider 决定：
//   - "edge"    → EdgeProvider（Microsoft Edge TTS WebSocket，无需下载，立即可用）
//   - "http"    → HTTPProvider（外部 sidecar，如 CosyVoice 2 / Qwen3-TTS）
//   - ""/"sherpa" → SherpaProvider（sherpa-onnx 本地 Kokoro，异步下载后激活）
func initTTSEngine(ctx context.Context, s *server.Server, dataDir string, gate *probe.FeatureGate, httpClient *http.Client, ttsConfig config.TTSConfig) {
	switch ttsConfig.Provider {
	case "edge":
		// Edge TTS：免费、无需下载、立即激活，不受 FeatureGate 门控（无内存开销）
		p := tts.NewEdgeProvider(ttsConfig.EdgeVoice)
		s.SetTTSProvider(&ttsAdapter{inner: p})
		slog.Info("tts: Edge TTS active", "voice", ttsConfig.EdgeVoice)
		return

	case "http":
		// HTTP sidecar：同样立即激活，连通性由首次调用时发现
		if ttsConfig.HTTPEndpoint == "" {
			slog.Warn("tts: provider=http but http_endpoint is empty, TTS disabled")
			return
		}
		p := tts.NewHTTPProvider(ttsConfig.HTTPEndpoint, httpClient)
		s.SetTTSProvider(&ttsAdapter{inner: p})
		slog.Info("tts: HTTP sidecar TTS active", "endpoint", ttsConfig.HTTPEndpoint)
		return
	}

	// ── Sherpa 本地路径（provider="" 或 "sherpa"）──────────────────────────────
	// 修复 bug：原代码错误使用 FeatureLocalSTT 门控 TTS，现改为独立的 FeatureLocalTTS。
	if gate != nil && gate.State(probe.FeatureLocalTTS) == probe.FeatureDisabled {
		slog.Info("tts: FeatureLocalTTS disabled by FeatureGate (need ≥512MB free)")
		return
	}
	if ttsConfig.ModelURL == "" {
		slog.Info("tts: sherpa provider but model_url is empty, TTS disabled")
		return
	}

	ttsDir := filepath.Join(dataDir, "models", "kokoro")
	//nolint:bare-goroutine // 历史代码暂留，需结合上下文梳理 ctx 传递链路，后续重构替换
	go func() {
		sttDir := filepath.Join(dataDir, "models", "sensevoice")
		if err := tts.EnsureAssets(ctx, sttDir, ttsDir, httpClient, ttsConfig.SherpaVersion, ttsConfig.ModelURL); err != nil {
			slog.Warn("tts: asset download failed", "err", err)
			return
		}

		libPath := filepath.Join(sttDir, stt.LibName())
		if err := tts.LoadLibrary(libPath); err != nil {
			slog.Warn("tts: library load failed", "err", err)
			return
		}

		modelDir := tts.ModelDir(ttsDir)
		engine, err := tts.NewEngine(modelDir)
		if err != nil {
			slog.Warn("tts: engine init failed", "err", err)
			return
		}
		s.SetTTSProvider(&ttsAdapter{inner: engine})
		slog.Info("tts: sherpa-onnx Kokoro active", "model_dir", modelDir)
	}()
}

// sttAdapter 将 llm/stt.Engine 适配为 chat.STTTranscriber
type sttAdapter struct {
	inner *stt.Engine
}

func (a *sttAdapter) Transcribe(samples []float32, sampleRate int) (chat.STTResult, error) {
	if a.inner == nil {
		return chat.STTResult{}, fmt.Errorf("stt engine not initialized")
	}
	res, err := a.inner.Transcribe(samples, sampleRate)
	if err != nil {
		return chat.STTResult{}, fmt.Errorf("transcribe failed: %w", err)
	}
	return chat.STTResult{
		Text: res.Text,
	}, nil
}

func (a *sttAdapter) IsAvailable() bool {
	return a.inner != nil
}

// ttsAdapter 将 llm/tts.Provider 适配为 chat.TTSProvider
type ttsAdapter struct {
	inner tts.Provider
}

func (a *ttsAdapter) Generate(ctx context.Context, text string) ([]byte, error) {
	if a.inner == nil {
		return nil, fmt.Errorf("tts provider not initialized")
	}
	res, err := a.inner.Generate(ctx, text)
	if err != nil {
		return nil, fmt.Errorf("generate failed: %w", err)
	}
	return res, nil
}

func (a *ttsAdapter) IsAvailable() bool {
	return a.inner != nil
}
