package main

import (
	"context"
	"log/slog"
	"net/http"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/gateway/server"
	"github.com/polarisagi/polaris/internal/gateway/server/chat"
	"github.com/polarisagi/polaris/internal/llm/audiorun"
	"github.com/polarisagi/polaris/internal/llm/stt"
	"github.com/polarisagi/polaris/internal/llm/tts"
	"github.com/polarisagi/polaris/internal/observability/probe"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
)

// audioInit 是语音装配所需的全部输入。
type audioInit struct {
	Server        *server.Server
	DataDir       string
	TierParams    *probe.TierParameters // 可 nil（无 AutoConf）
	HTTPClient    *http.Client
	Cfg           config.InferenceConfig
	Prefs         audiorun.PrefStore // 持久化首次 TTS 基准（HE-6）
	TotalRAMBytes uint64
}

// audioRuntime 聚合语音服务，实现 chat.AudioInstaller（POST /v1/audio/{stt|tts}/install 的后端）。
type audioRuntime struct {
	stt *audiorun.STTService
	tts *audiorun.TTSService // provider="http" 时为 nil（外部 sidecar 无需安装）
}

// Install 实现 chat.AudioInstaller。
func (a *audioRuntime) Install(kind string) (bool, error) {
	switch kind {
	case "stt":
		return a.stt.Install() //nolint:wrapcheck // audiorun 已用 apperr 包装
	case "tts":
		if a.tts == nil {
			return false, nil
		}
		return a.tts.Install() //nolint:wrapcheck // audiorun 已用 apperr 包装
	}
	return false, apperr.New(apperr.CodeInvalidInput, "unknown audio kind: "+kind)
}

// Close 卸载全部引擎（进程关停时调用）。
func (a *audioRuntime) Close() {
	a.stt.Close()
	if a.tts != nil {
		if err := a.tts.Close(); err != nil {
			slog.Warn("audio: tts close failed", "err", err)
		}
	}
}

// initAudio 装配 STT/TTS 语音服务（ADR-0107）：
//   - 不在启动时下载任何语音资产：状态机初始为 not_installed / unsupported / ready（已装但未加载），
//     由前端首次使用时调用 install 触发下载；
//   - 引擎首次请求时才加载，空闲 inference.audio.idle_unload_minutes 分钟后卸载；
//   - "是否支持"只看稳定硬件画像（总内存/逻辑核/arch），空闲内存只决定此刻能否加载。
func initAudio(ctx context.Context, in audioInit) *audioRuntime {
	s := in.Server
	profile := audiorun.HostProfile(in.TotalRAMBytes)
	support := audiorun.AudioSupport(profile)
	idle := time.Duration(in.Cfg.Audio.IdleUnloadMinutes) * time.Minute
	sttDir := filepath.Join(in.DataDir, "models", "sensevoice")

	sttThreads := 1
	if in.TierParams != nil && in.TierParams.STTNumThreads > 0 {
		sttThreads = in.TierParams.STTNumThreads
	}
	// 本包已有同名 min（float64 版）遮蔽内建，这里手写钳位。
	ttsThreads := runtime.NumCPU()
	if ttsThreads > 4 {
		ttsThreads = 4
	}
	if ttsThreads < 1 {
		ttsThreads = 1
	}
	if in.TierParams != nil && in.TierParams.TTSNumThreads > 0 {
		ttsThreads = in.TierParams.TTSNumThreads
	}

	slog.Info("audio: support judged from stable hardware profile",
		"total_ram_mb", in.TotalRAMBytes/(1024*1024), "cores", profile.LogicalCores, "arch", profile.GOARCH,
		"stt_supported", support.STT.Supported, "tts_supported", support.TTS.Supported,
		"idle_unload", idle.String())

	rt := &audioRuntime{}
	rt.stt = audiorun.NewSTTService(ctx, audiorun.STTOptions{
		Dir:           sttDir,
		SherpaVersion: in.Cfg.STT.SherpaVersion,
		Language:      in.Cfg.STT.Language,
		NumThreads:    sttThreads,
		UseITN:        in.Cfg.STT.UseITN,
		IdleUnload:    idle,
		HTTPClient:    in.HTTPClient,
		FreeMemMB:     probe.ProbeAvailableMemoryMB,
		Support:       support.STT,
		Sink:          audioSink{publish: s.PublishSTTStatus},
	})
	s.SetSTTProvider(&sttAdapter{inner: rt.stt})

	initTTS(ctx, in, rt, profile, support.TTS, idle, ttsThreads, sttDir)

	s.SetTTSEnginePref(in.Cfg.TTS.Engine)
	s.SetAudioInstaller(rt)
	// 进程关停时释放原生引擎（推理中的请求持有引用，Slot.Close 不会在其使用期间卸载）。
	concurrent.SafeGo(ctx, "server_stt_tts.close_on_shutdown", func(ctx context.Context) {
		<-ctx.Done()
		rt.Close()
	})
	return rt
}

// initTTS 按 provider 装配 TTS：http sidecar 立即就绪；sherpa 走按需安装 + 懒加载 + 首次基准。
func initTTS(ctx context.Context, in audioInit, rt *audioRuntime, profile audiorun.HardwareProfile,
	support audiorun.Capability, idle time.Duration, threads int, libDir string) {
	s := in.Server
	cfg := in.Cfg.TTS

	if cfg.Provider == "http" {
		if cfg.HTTPEndpoint == "" {
			slog.Warn("tts: provider=http but http_endpoint is empty, TTS disabled")
			s.PublishTTSStatus(chat.AudioAssetStatus{State: chat.AudioStateDisabled, Detail: "provider=http 但 http_endpoint 为空"})
			return
		}
		s.SetTTSProvider(&ttsAdapter{inner: tts.NewHTTPProvider(cfg.HTTPEndpoint, in.HTTPClient)}, "http")
		s.PublishTTSStatus(chat.AudioAssetStatus{State: chat.AudioStateReady, Detail: "HTTP sidecar"})
		slog.Info("tts: HTTP sidecar TTS active", "endpoint", cfg.HTTPEndpoint)
		return
	}

	rt.tts = audiorun.NewTTSService(ctx, audiorun.TTSOptions{
		LibDir:        libDir,
		Dir:           filepath.Join(in.DataDir, "models", "kokoro"),
		SherpaVersion: cfg.SherpaVersion,
		SID:           int32(cfg.KokoroSID),
		Speed:         float32(cfg.Speed),
		NumThreads:    threads,
		IdleUnload:    idle,
		HTTPClient:    in.HTTPClient,
		FreeMemMB:     probe.ProbeAvailableMemoryMB,
		Support:       support,
		Sink:          audioSink{publish: s.PublishTTSStatus},
		Prefs:         in.Prefs,
		Profile:       profile,
	})
	s.SetTTSProvider(&ttsAdapter{inner: rt.tts}, "sherpa")
}

// audioSink 把 audiorun 的状态快照适配为 chat 状态机的快照（audiorun 不 import gateway 层）。
type audioSink struct {
	publish func(chat.AudioAssetStatus)
}

func (k audioSink) Publish(st audiorun.Status) { k.publish(toChatStatus(st)) }

// toChatStatus 做字段映射。状态字符串两侧字面一致，由 server_stt_tts_test.go 守住不漂移。
func toChatStatus(st audiorun.Status) chat.AudioAssetStatus {
	out := chat.AudioAssetStatus{
		State: st.State, Detail: st.Detail, Error: st.Error, Reason: st.Reason,
		InstallSizeBytes: st.InstallSizeBytes, Loaded: st.Loaded,
	}
	if st.BytesDone > 0 || st.BytesTotal > 0 {
		out.Progress = &chat.AudioProgress{BytesDone: st.BytesDone, BytesTotal: st.BytesTotal}
	}
	return out
}

// sttTranscriber 是 sttAdapter 对转写引擎的依赖（由 *audiorun.STTService 满足）。
type sttTranscriber interface {
	Transcribe(samples []float32, sampleRate int) (stt.Result, error)
}

// sttAdapter 将转写服务适配为 chat.STTTranscriber。
type sttAdapter struct {
	inner sttTranscriber
}

func (a *sttAdapter) Transcribe(samples []float32, sampleRate int) (chat.STTResult, error) {
	if a.inner == nil {
		return chat.STTResult{}, apperr.New(apperr.CodeUnimplemented, "stt engine not initialized")
	}
	res, err := a.inner.Transcribe(samples, sampleRate)
	if err != nil {
		if _, ok := audiorun.AsNotReady(err); ok {
			return chat.STTResult{}, err //nolint:wrapcheck // 原样透传：HTTP 层据此给出 503 + 原因，不能被包成 500
		}
		return chat.STTResult{}, apperr.Wrap(apperr.CodeInternal, "transcribe failed", err)
	}
	return chat.STTResult{Text: res.Text, Language: trimLangTag(res.Lang)}, nil
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
		if _, ok := audiorun.AsNotReady(err); ok {
			return chat.TTSAudio{}, err //nolint:wrapcheck // NotReady 原样透传，HTTP 层据此给 503
		}
		return chat.TTSAudio{}, apperr.Wrap(apperr.CodeInternal, "generate failed", err)
	}
	return chat.TTSAudio{Data: res.Data, MIME: res.MIME}, nil
}

// ttsBridge 是 tts 内置工具与 TTS 引擎之间的晚绑定：工具在 bootTools 注册，
// 而承载引擎的 Server 到 bootServer 才创建，两者之间只能经此桥接。
type ttsBridge struct {
	fn atomic.Pointer[func(ctx context.Context, text string) (chat.TTSAudio, error)]
}

// Bind 在 Server 创建后接入合成函数。
func (b *ttsBridge) Bind(fn func(ctx context.Context, text string) (chat.TTSAudio, error)) {
	b.fn.Store(&fn)
}

// Synthesize 满足 tts 工具的 Synthesizer 签名；未 Bind 时如实报错，不回出假音频。
func (b *ttsBridge) Synthesize(ctx context.Context, text string) ([]byte, string, error) {
	fn := b.fn.Load()
	if fn == nil {
		return nil, "", apperr.New(apperr.CodeUnimplemented, "tts: 引擎尚未接线（服务启动中）")
	}
	a, err := (*fn)(ctx, text)
	if err != nil {
		return nil, "", err
	}
	return a.Data, a.MIME, nil
}

// trimLangTag 去掉 SenseVoice 语言标签的 "<|" "|>" 包裹（"<|yue|>" -> "yue"）。
// 这是模型内部 token 的写法，不应泄露到 API；空串原样返回，由 JSON omitempty 省略该字段。
func trimLangTag(l string) string {
	return strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(l), "<|"), "|>"))
}
