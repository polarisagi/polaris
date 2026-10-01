package audiorun

import (
	"context"
	"log/slog"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/polarisagi/polaris/internal/llm/stt"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
)

// STTOptions 是 STTService 的构造参数。
type STTOptions struct {
	Dir           string // STT 资产根目录（含动态库 + model/ + punct_model/）
	SherpaVersion string
	Language      string
	NumThreads    int
	UseITN        bool
	IdleUnload    time.Duration
	LoadWait      time.Duration // <=0 取 DefaultLoadWait
	HTTPClient    *http.Client
	// FreeMemMB 返回当前空闲内存（MB）；加载前检查，不足则 insufficient_memory。
	FreeMemMB func() uint64
	Support   Capability
	Sink      StatusSink
}

// STTService 把 stt.Engine 包成"按需安装、懒加载、空闲卸载"的服务，实现转写接口。
type STTService struct {
	o          STTOptions
	rootCtx    context.Context
	slot       *Slot[*stt.Engine]
	installing atomic.Bool
}

// NewSTTService 构造服务并发布初始状态。ctx 是守护进程生命周期（后台安装用它，而非请求的 ctx）。
func NewSTTService(ctx context.Context, o STTOptions) *STTService {
	if o.Sink == nil {
		o.Sink = nopSink{}
	}
	if o.FreeMemMB == nil {
		o.FreeMemMB = func() uint64 { return ^uint64(0) }
	}
	s := &STTService{o: o, rootCtx: ctx}
	s.slot = NewSlot(SlotConfig[*stt.Engine]{
		Name:       "stt",
		Load:       s.loadEngine,
		Unload:     func(e *stt.Engine) { e.Close() },
		IdleUnload: o.IdleUnload,
		LoadWait:   o.LoadWait,
		Hooks: SlotHooks{
			OnLoading: func() {
				o.Sink.Publish(Status{State: StateLoading, Detail: "加载语音识别引擎"})
			},
			OnLoaded: func() {
				slog.Info("audio: stt engine loaded")
				o.Sink.Publish(s.base(true))
			},
			OnUnloaded: func() {
				slog.Info("audio: stt engine unloaded (idle)", "idle", o.IdleUnload.String())
				o.Sink.Publish(s.base(false))
			},
			OnLoadFailed: s.onLoadFailed,
		},
	})
	o.Sink.Publish(s.base(false))
	return s
}

// base 计算"无进行中操作"时的状态：不支持 / 未安装 / 就绪。
func (s *STTService) base(loaded bool) Status {
	if !s.o.Support.Supported {
		return Status{State: StateUnsupported, Reason: s.o.Support.Reason, Detail: s.o.Support.Message}
	}
	if missing := stt.MissingAssets(s.o.Dir); len(missing) > 0 {
		return Status{
			State: StateNotInstalled, InstallSizeBytes: sumSize(missing),
			Detail: "语音识别模型未安装，首次使用时下载（约 " + humanBytes(sumSize(missing)) + "，离线运行）",
		}
	}
	if loaded {
		return Status{State: StateReady, Loaded: true, Detail: "引擎已加载"}
	}
	return Status{State: StateReady, Detail: "已安装，首次使用时加载，空闲后自动卸载"}
}

func (s *STTService) onLoadFailed(err error) {
	if nr, ok := AsNotReady(err); ok {
		// 内存不足是瞬时状态：资产仍然就绪，只是此刻加不动，请求侧会拿到明确的 503 原因。
		slog.Warn("audio: stt engine load refused", "code", nr.Code, "msg", nr.Message)
		s.o.Sink.Publish(s.base(false))
		return
	}
	slog.Error("audio: stt engine load failed", "err", err)
	s.o.Sink.Publish(Status{State: StateFailed, Error: err.Error()})
}

// loadEngine 由 Slot 在后台调用：内存检查 → 加载动态库 → 创建引擎。
func (s *STTService) loadEngine(_ context.Context) (*stt.Engine, error) {
	if free := s.o.FreeMemMB(); free < sttMinFreeMB {
		return nil, notReady(CodeInsufficientMemory, memMsg("语音识别", free, sttMinFreeMB))
	}
	if err := stt.LoadLibrary(filepath.Join(s.o.Dir, stt.LibName())); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "stt: 动态库加载失败"+dlopenHint(), err)
	}
	lang := s.o.Language
	if lang == "" {
		lang = "zh"
	}
	eng, err := stt.NewEngine(stt.ModelDir(s.o.Dir), stt.PunctModelDir(s.o.Dir), lang, s.o.NumThreads, s.o.UseITN)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "stt: engine init failed", err)
	}
	slog.Info("audio: stt engine created (sherpa-onnx SenseVoice int8)",
		"language", lang, "threads", s.o.NumThreads, "use_itn", s.o.UseITN)
	return eng, nil
}

// gate 在加载前拦截"根本不可能服务"的状态，给出可向用户解释的原因。
func (s *STTService) gate() error {
	if !s.o.Support.Supported {
		return notReady(CodeUnsupported, s.o.Support.Message)
	}
	if s.installing.Load() {
		return notReady(CodeInstalling, "语音识别模型正在下载，请稍候")
	}
	if missing := stt.MissingAssets(s.o.Dir); len(missing) > 0 {
		return notReady(CodeNotInstalled, "语音识别模型未安装（约 "+humanBytes(sumSize(missing))+"），请先启用语音输入")
	}
	return nil
}

// Transcribe 转写 PCM 采样。未安装/不支持/内存不足返回 *NotReadyError；已卸载则先重新加载。
func (s *STTService) Transcribe(samples []float32, sampleRate int) (stt.Result, error) {
	if err := s.gate(); err != nil {
		return stt.Result{}, err
	}
	eng, release, err := s.slot.Acquire(context.Background())
	if err != nil {
		return stt.Result{}, err
	}
	defer release()
	res, err := eng.Transcribe(samples, sampleRate)
	if err != nil {
		return stt.Result{}, apperr.Wrap(apperr.CodeInternal, "stt: transcribe failed", err)
	}
	return res, nil
}

// Install 启动后台安装（下载 + sha256 校验）。返回 started=false 表示无需或已在进行中。
// 不支持的机器返回 *NotReadyError（unsupported）。
func (s *STTService) Install() (started bool, err error) {
	if !s.o.Support.Supported {
		return false, notReady(CodeUnsupported, s.o.Support.Message)
	}
	if len(stt.MissingAssets(s.o.Dir)) == 0 {
		return false, nil
	}
	if !s.installing.CompareAndSwap(false, true) {
		return false, nil
	}
	s.o.Sink.Publish(Status{State: StateDownloading, Detail: "准备下载语音识别模型"})
	concurrent.SafeGo(s.rootCtx, "audiorun.stt_install", func(ctx context.Context) {
		defer s.installing.Store(false)
		if err := stt.EnsureAssets(ctx, s.o.Dir, s.o.HTTPClient, s.o.SherpaVersion, downloadProgress(s.o.Sink)); err != nil {
			slog.Error("audio: stt install failed", "err", err)
			s.o.Sink.Publish(Status{State: StateFailed, Error: err.Error()})
			return
		}
		slog.Info("audio: stt install complete", "dir", s.o.Dir)
		s.o.Sink.Publish(s.base(s.slot.IsResident()))
	})
	return true, nil
}

// Close 关闭服务并卸载引擎。
func (s *STTService) Close() { s.slot.Close() }

// IsResident 报告引擎是否驻留内存（测试与诊断用）。
func (s *STTService) IsResident() bool { return s.slot.IsResident() }
