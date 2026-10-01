package main

import (
	"context"
	"log/slog"
	"net/http"
	"path/filepath"
	"runtime"
	"time"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/gateway/server"
	"github.com/polarisagi/polaris/internal/gateway/server/chat"
	"github.com/polarisagi/polaris/internal/llm/stt"
	"github.com/polarisagi/polaris/internal/llm/tts"
	"github.com/polarisagi/polaris/internal/observability/probe"
	"github.com/polarisagi/polaris/internal/security/network"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
)

// sttRetryBackoff 返回第 attempt 次失败（从 0 起）后的退避时长：1m → 5m → 15m → 之后每 1h。
func sttRetryBackoff(attempt int) time.Duration {
	steps := []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute}
	if attempt < len(steps) {
		return steps[attempt]
	}
	return time.Hour
}

// initSTTEngine 按 FeatureGate 门控初始化 STT 引擎。
// 流程：
//  1. 门控禁用 → 状态置 disabled 后返回（不存在任何"mock 引擎"：引擎未就绪时转写接口返回 503 JSON）
//  2. 否则后台循环：EnsureAssets → LoadLibrary → NewEngine → 注入真实引擎；
//     任一步失败 → 状态 failed + 退避重试（1m/5m/15m/之后每 1h）；
//     failed 期间收到转写请求会唤醒立即重试（见 AudioService.kickSTTRetry）。
func initSTTEngine(ctx context.Context, s *server.Server, dataDir string, gate *probe.FeatureGate, params *probe.TierParameters, httpClient *http.Client, sttConfig config.STTConfig) {
	sttDir := filepath.Join(dataDir, "models", "sensevoice")

	// 门控检查：FeatureLocalSTT 是最低档（int8），未开启则无法运行 STT
	if gate != nil && gate.State(probe.FeatureLocalSTT) == probe.FeatureDisabled {
		slog.Info("stt: FeatureLocalSTT disabled by FeatureGate (need ≥512MB free)")
		s.SetSTTStatus(chat.AudioStateDisabled, "可用内存不足 512MB，语音识别被禁用", "")
		return
	}

	// 默认 int8（166MB）；仅 model_precision="fp32" 且 FeatureHQSTT 开启时才用 fp32（886MB）。
	// 旧逻辑"HQ 门控开启就自动选 fp32"会让首次使用下载 1.16GB，而 int8 实测中文识别正确。
	useFP32 := sttConfig.ModelPrecision == "fp32" && gate != nil && gate.State(probe.FeatureHQSTT) != probe.FeatureDisabled
	modelURL := sttConfig.SenseVoiceModelURLStd
	if useFP32 || modelURL == "" {
		modelURL = sttConfig.SenseVoiceModelURL
	}

	numThreads := 1
	if params != nil && params.STTNumThreads > 0 {
		numThreads = params.STTNumThreads
	}
	lang := sttConfig.Language
	if lang == "" {
		lang = "zh"
	}

	s.SetSTTStatus(chat.AudioStatePending, "等待准备语音识别资产", "")

	p := sttPrep{
		s: s, sttDir: sttDir, httpClient: httpClient, cfg: sttConfig,
		modelURL: modelURL, lang: lang, numThreads: numThreads, useFP32: useFP32,
	}
	concurrent.SafeGo(ctx, "server_stt_tts.stt_prepare", func(ctx context.Context) {
		p.loop(ctx)
	})
}

// loop 反复执行 run 直到成功或 ctx 取消：失败 → 状态 failed + 退避；
// 退避期间若收到转写请求的唤醒信号则立即重试。
func (p sttPrep) loop(ctx context.Context) {
	retry := p.s.STTRetrySignal()
	for attempt := 0; ; attempt++ {
		err := p.run(ctx)
		if err == nil || ctx.Err() != nil {
			return
		}
		wait := sttRetryBackoff(attempt)
		slog.Warn("stt: asset preparation failed, will retry", "err", err, "retry_in", wait.String())
		p.s.SetSTTStatus(chat.AudioStateFailed, "", err.Error())

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		case <-retry:
			timer.Stop()
			slog.Info("stt: retry triggered by a transcription request")
		}
	}
}

// sttPrep 封装一次 STT 资产准备所需的全部输入，使重试循环与单次准备解耦。
type sttPrep struct {
	s          *server.Server
	sttDir     string
	httpClient *http.Client
	cfg        config.STTConfig
	modelURL   string
	lang       string
	numThreads int
	useFP32    bool
}

// run 执行一次完整准备：EnsureAssets → LoadLibrary → NewEngine → 注入真实引擎。
// 每步通过状态机汇报当前步骤；任一步失败返回 error（由调用方退避重试）。
func (p sttPrep) run(ctx context.Context) error {
	p.s.SetSTTStatus(chat.AudioStateDownloading, "检查语音识别资产", "")
	onStep := func(step string) { p.s.SetSTTStatus(chat.AudioStateDownloading, step, "") }
	if err := stt.EnsureAssets(ctx, p.sttDir, p.httpClient, p.cfg.SherpaVersion, p.modelURL, p.cfg.PunctModelURL, onStep); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "stt: ensure assets failed", err)
	}

	p.s.SetSTTStatus(chat.AudioStateDownloading, "加载引擎", "")
	if err := stt.LoadLibrary(filepath.Join(p.sttDir, stt.LibName())); err != nil {
		if runtime.GOOS == "darwin" {
			// sherpa-onnx 1.13.2 自带 libonnxruntime（arm64 与 x64）minos=15.5，
			// macOS < 15.5 上 dlopen 必然失败，给出可操作的原因而不是裸 dlopen 错误。
			return apperr.Wrap(apperr.CodeInternal, "stt: 动态库加载失败（需要 macOS ≥ 15.5：onnxruntime 1.24.4 minos）", err)
		}
		return apperr.Wrap(apperr.CodeInternal, "stt: 动态库加载失败", err)
	}

	modelDir := stt.ModelDir(p.sttDir)
	engine, err := stt.NewEngine(modelDir, stt.PunctModelDir(p.sttDir), p.lang, p.numThreads, p.cfg.UseITN)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "stt: engine init failed", err)
	}
	p.s.SetSTTProvider(&sttAdapter{inner: engine})
	p.s.SetSTTStatus(chat.AudioStateReady, "", "")
	slog.Info("stt: real engine active (sherpa-onnx SenseVoice)",
		"model_dir", modelDir,
		"fp32", p.useFP32,
		"model_url", p.modelURL,
		"language", p.lang,
		"threads", p.numThreads,
		"use_itn", p.cfg.UseITN,
	)
	return nil
}

// initTTSEngine 初始化 TTS Provider 并注入 ChatHandler。
//
// 三条路径由 ttsConfig.Provider 决定：
//   - "edge"    → EdgeProvider（Microsoft Edge TTS WebSocket，无需下载，立即可用）
//   - "http"    → HTTPProvider（外部 sidecar，如 CosyVoice 2 / Qwen3-TTS）
//   - ""/"sherpa" → SherpaProvider（sherpa-onnx 本地 Kokoro，异步下载后激活）
func initTTSEngine(ctx context.Context, s *server.Server, dataDir string, gate *probe.FeatureGate, params *probe.TierParameters, httpClient *http.Client, ttsConfig config.TTSConfig, safeDialer *network.SafeDialer) {
	switch ttsConfig.Provider {
	case "edge":
		// Edge TTS：免费、无需下载、立即激活，不受 FeatureGate 门控（无内存开销）。
		// 它是在线服务，状态恒为 ready；单次合成失败只记日志，不改状态。
		if ttsConfig.EdgeStyle != "" && ttsConfig.EdgeStyle != "default" {
			slog.Warn("tts: Edge 免费端点不支持 express-as（服务端返回 SSML is invalid），edge_style 已忽略",
				"edge_style", ttsConfig.EdgeStyle)
		}
		p := tts.NewEdgeProvider(ttsConfig.EdgeVoice, ttsConfig.EdgeClientVersion, safeDialer)
		s.SetTTSProvider(&ttsAdapter{inner: p}, "edge")
		s.SetTTSStatus(chat.AudioStateReady, "Edge TTS（在线）", "")
		slog.Info("tts: Edge TTS active", "voice", ttsConfig.EdgeVoice)
		return

	case "http":
		// HTTP sidecar：同样立即激活，连通性由首次调用时发现
		if ttsConfig.HTTPEndpoint == "" {
			slog.Warn("tts: provider=http but http_endpoint is empty, TTS disabled")
			s.SetTTSStatus(chat.AudioStateDisabled, "provider=http 但 http_endpoint 为空", "")
			return
		}
		p := tts.NewHTTPProvider(ttsConfig.HTTPEndpoint, httpClient)
		s.SetTTSProvider(&ttsAdapter{inner: p}, "http")
		s.SetTTSStatus(chat.AudioStateReady, "HTTP sidecar", "")
		slog.Info("tts: HTTP sidecar TTS active", "endpoint", ttsConfig.HTTPEndpoint)
		return
	}

	// ── Sherpa 本地路径（provider="" 或 "sherpa"）──────────────────────────────
	// 修复 bug：原代码错误使用 FeatureLocalSTT 门控 TTS，现改为独立的 FeatureLocalTTS。
	if gate != nil && gate.State(probe.FeatureLocalTTS) == probe.FeatureDisabled {
		slog.Info("tts: FeatureLocalTTS disabled by FeatureGate (need ≥512MB free)")
		s.SetTTSStatus(chat.AudioStateDisabled, "可用内存不足 512MB，本地朗读被禁用", "")
		return
	}
	if ttsConfig.ModelURL == "" {
		slog.Info("tts: sherpa provider but model_url is empty, TTS disabled")
		s.SetTTSStatus(chat.AudioStateDisabled, "sherpa provider 未配置 model_url", "")
		return
	}

	ttsNumThreads := 2
	if params != nil && params.TTSNumThreads > 0 {
		ttsNumThreads = params.TTSNumThreads
	}

	s.SetTTSStatus(chat.AudioStatePending, "等待准备本地朗读资产", "")
	ttsDir := filepath.Join(dataDir, "models", "kokoro")
	concurrent.SafeGo(ctx, "server_stt_tts.tts_download", func(ctx context.Context) {
		fail := func(msg string, err error) {
			slog.Warn(msg, "err", err)
			s.SetTTSStatus(chat.AudioStateFailed, "", err.Error())
		}
		s.SetTTSStatus(chat.AudioStateDownloading, "下载本地朗读资产", "")
		sttDir := filepath.Join(dataDir, "models", "sensevoice")
		if err := tts.EnsureAssets(ctx, sttDir, ttsDir, httpClient, ttsConfig.SherpaVersion, ttsConfig.ModelURL); err != nil {
			fail("tts: asset download failed", err)
			return
		}

		libPath := filepath.Join(sttDir, stt.LibName())
		if err := tts.LoadLibrary(libPath); err != nil {
			fail("tts: library load failed", err)
			return
		}

		modelDir := tts.ModelDir(ttsDir)
		engine, err := tts.NewEngine(modelDir, ttsNumThreads)
		if err != nil {
			fail("tts: engine init failed", err)
			return
		}
		s.SetTTSProvider(&ttsAdapter{inner: engine}, "sherpa")
		s.SetTTSStatus(chat.AudioStateReady, "", "")
		slog.Info("tts: sherpa-onnx Kokoro active", "model_dir", modelDir, "threads", ttsNumThreads)
	})
}

// sttAdapter 将 llm/stt.Engine 适配为 chat.STTTranscriber
type sttAdapter struct {
	inner *stt.Engine
}

func (a *sttAdapter) Transcribe(samples []float32, sampleRate int) (chat.STTResult, error) {
	if a.inner == nil {
		return chat.STTResult{}, apperr.New(apperr.CodeUnimplemented, "stt engine not initialized")
	}
	res, err := a.inner.Transcribe(samples, sampleRate)
	if err != nil {
		return chat.STTResult{}, apperr.Wrap(apperr.CodeInternal, "transcribe failed", err)
	}
	return chat.STTResult{
		Text:     res.Text,
		Language: res.Lang,
	}, nil
}

func (a *sttAdapter) IsAvailable() bool {
	return a.inner != nil
}

// ttsAdapter 将 llm/tts.Provider 适配为 chat.TTSProvider
type ttsAdapter struct {
	inner tts.Provider
}

func (a *ttsAdapter) Generate(ctx context.Context, text string) (chat.TTSAudio, error) {
	if a.inner == nil {
		return chat.TTSAudio{}, apperr.New(apperr.CodeUnimplemented, "tts provider not initialized")
	}
	res, err := a.inner.Generate(ctx, text)
	if err != nil {
		return chat.TTSAudio{}, apperr.Wrap(apperr.CodeInternal, "generate failed", err)
	}
	return chat.TTSAudio{Data: res.Data, MIME: res.MIME}, nil
}

func (a *ttsAdapter) IsAvailable() bool {
	return a.inner != nil
}
