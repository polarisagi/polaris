package audiorun

import (
	"context"
	"log/slog"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/polarisagi/polaris/internal/llm/stt"
	"github.com/polarisagi/polaris/internal/llm/tts"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
)

// TTSOptions 是 TTSService 的构造参数。
type TTSOptions struct {
	LibDir        string // sherpa 动态库目录（与 STT 共用，通常 = sttDir）
	Dir           string // TTS 资产根目录（模型在 Dir/model/）
	SherpaVersion string
	SID           int32
	Speed         float32
	NumThreads    int
	IdleUnload    time.Duration
	LoadWait      time.Duration // <=0 取 DefaultLoadWait
	HTTPClient    *http.Client
	FreeMemMB     func() uint64
	Support       Capability
	Sink          StatusSink
	// Prefs 持久化首次基准结果（HE-6）；nil 时每次首次加载都重跑基准且不落库。
	Prefs PrefStore
	// Profile 用于计算硬件指纹（指纹变化才重测）。
	Profile HardwareProfile
}

// TTSService 把 tts.Engine（Kokoro）包成"按需安装、懒加载、首次基准、空闲卸载"的服务，实现 tts.Provider。
type TTSService struct {
	o          TTSOptions
	rootCtx    context.Context
	fp         string
	slot       *Slot[*tts.Engine]
	installing atomic.Bool
	// slow 非 nil 表示本机已被基准判定为过慢（同指纹）：服务端 TTS 置 unsupported，前端用系统语音。
	slow atomic.Pointer[BenchRecord]
}

var _ tts.Provider = (*TTSService)(nil)

// NewTTSService 构造服务并发布初始状态（含读取持久化的基准结论）。
func NewTTSService(ctx context.Context, o TTSOptions) *TTSService {
	if o.Sink == nil {
		o.Sink = nopSink{}
	}
	if o.FreeMemMB == nil {
		o.FreeMemMB = func() uint64 { return ^uint64(0) }
	}
	t := &TTSService{o: o, rootCtx: ctx, fp: Fingerprint(o.Profile)}
	t.slot = NewSlot(SlotConfig[*tts.Engine]{
		Name: "tts",
		Load: t.loadEngine,
		Unload: func(e *tts.Engine) {
			if err := e.Close(); err != nil {
				slog.Warn("audio: tts engine close failed", "err", err)
			}
		},
		IdleUnload: o.IdleUnload,
		LoadWait:   o.LoadWait,
		Hooks: SlotHooks{
			OnLoading: func() {
				o.Sink.Publish(Status{State: StateLoading, Detail: "加载语音合成引擎"})
			},
			OnLoaded: func() {
				slog.Info("audio: tts engine loaded")
				o.Sink.Publish(t.base(true))
			},
			OnUnloaded: func() {
				slog.Info("audio: tts engine unloaded (idle)", "idle", o.IdleUnload.String())
				o.Sink.Publish(t.base(false))
			},
			OnLoadFailed: t.onLoadFailed,
		},
	})
	t.restoreBench(ctx)
	o.Sink.Publish(t.base(false))
	return t
}

// restoreBench 读取持久化的基准结论：同指纹且判定过慢则直接标 unsupported，不必再加载 600MB 模型。
func (t *TTSService) restoreBench(ctx context.Context) {
	rec, ok, err := GetBench(ctx, t.o.Prefs, t.fp)
	if err != nil {
		slog.Warn("audio: tts bench record unreadable, will re-measure on first load", "err", err)
		return
	}
	if ok && !rec.Supported {
		t.slow.Store(&rec)
		slog.Info("audio: tts previously judged too slow on this hardware", "rtf", rec.RTF, "fingerprint", rec.Fingerprint)
	}
}

func (t *TTSService) tooSlowStatus() Status {
	rec := t.slow.Load()
	return Status{
		State: StateUnsupported, Reason: ReasonTooSlow,
		Detail: "本机服务端语音合成速度不足（实时率 " + rtfText(rec.RTF) + " > " + rtfText(MaxTTSRTF) + "），朗读改用系统语音",
	}
}

// base 计算"无进行中操作"时的状态：不支持 / 过慢 / 未安装 / 就绪。
func (t *TTSService) base(loaded bool) Status {
	if !t.o.Support.Supported {
		return Status{State: StateUnsupported, Reason: t.o.Support.Reason, Detail: t.o.Support.Message}
	}
	if t.slow.Load() != nil {
		return t.tooSlowStatus()
	}
	if missing := tts.MissingAssets(t.o.LibDir, t.o.Dir); len(missing) > 0 {
		return Status{
			State: StateNotInstalled, InstallSizeBytes: sumSize(missing),
			Detail: "语音合成模型未安装，首次使用时下载（约 " + humanBytes(sumSize(missing)) + "，离线运行）",
		}
	}
	if loaded {
		return Status{State: StateReady, Loaded: true, Detail: "引擎已加载"}
	}
	return Status{State: StateReady, Detail: "已安装，首次使用时加载，空闲后自动卸载"}
}

func (t *TTSService) onLoadFailed(err error) {
	if nr, ok := AsNotReady(err); ok {
		slog.Warn("audio: tts engine load refused", "code", nr.Code, "msg", nr.Message)
		t.o.Sink.Publish(t.base(false)) // unsupported(too_slow) 已写入 slow，base 会给出对应状态
		return
	}
	slog.Error("audio: tts engine load failed", "err", err)
	t.o.Sink.Publish(Status{State: StateFailed, Error: err.Error()})
}

// loadEngine 由 Slot 在后台调用：内存检查 → 加载库 → 创建引擎 → （首次）基准。
func (t *TTSService) loadEngine(ctx context.Context) (*tts.Engine, error) {
	if free := t.o.FreeMemMB(); free < ttsMinFreeMB {
		return nil, notReady(CodeInsufficientMemory, memMsg("语音合成", free, ttsMinFreeMB))
	}
	if err := tts.LoadLibrary(filepath.Join(t.o.LibDir, stt.LibName())); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "tts: 动态库加载失败"+dlopenHint(), err)
	}
	eng, err := tts.NewEngine(tts.ModelDir(t.o.Dir), tts.Options{NumThreads: t.o.NumThreads, SID: t.o.SID, Speed: t.o.Speed})
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "tts: engine init failed", err)
	}
	slog.Info("audio: tts engine created (sherpa-onnx Kokoro v1.1 fp32)",
		"threads", t.o.NumThreads, "sid", t.o.SID, "speed", t.o.Speed)

	if _, ok, lerr := GetBench(ctx, t.o.Prefs, t.fp); lerr != nil {
		slog.Warn("audio: tts bench record unreadable, re-measuring", "err", lerr)
	} else if ok {
		return eng, nil // 同指纹已测过且支持（过慢的在 restoreBench 里就拦下了）
	}
	return t.firstBench(ctx, eng)
}

// firstBench 在首次加载后测 RTF 并持久化；过慢则卸载并置 unsupported(too_slow)。
func (t *TTSService) firstBench(ctx context.Context, eng *tts.Engine) (*tts.Engine, error) {
	t.o.Sink.Publish(Status{State: StateLoading, Detail: "首次启用：运行语音合成速度基准"})
	rtf, err := RunBench(ctx, eng)
	if err != nil {
		if cerr := eng.Close(); cerr != nil {
			slog.Warn("audio: tts engine close after bench failure failed", "err", cerr)
		}
		return nil, err
	}
	rec := BenchRecord{Fingerprint: t.fp, RTF: rtf, Supported: rtf <= MaxTTSRTF, MeasuredAt: time.Now().UTC()}
	slog.Info("audio: tts first-load benchmark",
		"rtf", rtf, "max_rtf", MaxTTSRTF, "supported", rec.Supported,
		"threads", t.o.NumThreads, "fingerprint", t.fp)
	if serr := SaveBench(ctx, t.o.Prefs, rec); serr != nil {
		// 落库失败不阻断本次使用（结果仍有效），但下次启动会重测——必须留痕。
		slog.Warn("audio: tts bench result not persisted", "err", serr)
	}
	if rec.Supported {
		return eng, nil
	}
	if cerr := eng.Close(); cerr != nil {
		slog.Warn("audio: tts engine close after slow bench failed", "err", cerr)
	}
	t.slow.Store(&rec)
	return nil, notReady(CodeUnsupported, t.tooSlowStatus().Detail)
}

// gate 在加载前拦截"根本不可能服务"的状态。
func (t *TTSService) gate() error {
	if !t.o.Support.Supported {
		return notReady(CodeUnsupported, t.o.Support.Message)
	}
	if t.slow.Load() != nil {
		return notReady(CodeUnsupported, t.tooSlowStatus().Detail)
	}
	if t.installing.Load() {
		return notReady(CodeInstalling, "语音合成模型正在下载，请稍候")
	}
	if missing := tts.MissingAssets(t.o.LibDir, t.o.Dir); len(missing) > 0 {
		return notReady(CodeNotInstalled, "语音合成模型未安装（约 "+humanBytes(sumSize(missing))+"），请先启用服务端朗读")
	}
	return nil
}

// Generate 实现 tts.Provider：未安装/不支持/内存不足返回 *NotReadyError；已卸载则先重新加载。
func (t *TTSService) Generate(ctx context.Context, text string) (tts.Audio, error) {
	if err := t.gate(); err != nil {
		return tts.Audio{}, err
	}
	eng, release, err := t.slot.Acquire(ctx)
	if err != nil {
		return tts.Audio{}, err
	}
	defer release()
	return eng.Generate(ctx, text) //nolint:wrapcheck // tts 包已用 apperr 包装
}

// Install 启动后台安装（下载 + sha256 校验 + 首次加载与基准）。
// started=false 表示无需或已在进行中；不支持的机器返回 *NotReadyError（unsupported）。
func (t *TTSService) Install() (started bool, err error) {
	if !t.o.Support.Supported {
		return false, notReady(CodeUnsupported, t.o.Support.Message)
	}
	if t.slow.Load() != nil {
		return false, notReady(CodeUnsupported, t.tooSlowStatus().Detail)
	}
	if len(tts.MissingAssets(t.o.LibDir, t.o.Dir)) == 0 {
		return false, nil
	}
	if !t.installing.CompareAndSwap(false, true) {
		return false, nil
	}
	t.o.Sink.Publish(Status{State: StateDownloading, Detail: "准备下载语音合成模型"})
	concurrent.SafeGo(t.rootCtx, "audiorun.tts_install", func(ctx context.Context) {
		defer t.installing.Store(false)
		if err := tts.EnsureAssets(ctx, t.o.LibDir, t.o.Dir, t.o.HTTPClient, t.o.SherpaVersion, downloadProgress(t.o.Sink)); err != nil {
			slog.Error("audio: tts install failed", "err", err)
			t.o.Sink.Publish(Status{State: StateFailed, Error: err.Error()})
			return
		}
		slog.Info("audio: tts install complete", "dir", t.o.Dir)
		t.warm(ctx)
	})
	return true, nil
}

// warm 安装完成后立即加载一次：既验证资产能真正跑起来，也触发首次基准（结果落库）。
// 失败状态已由 Slot 的 OnLoadFailed 钩子发布，这里只需把终态补齐。
func (t *TTSService) warm(ctx context.Context) {
	_, release, err := t.slot.Acquire(ctx)
	if err != nil {
		slog.Warn("audio: tts post-install warm-up did not yield a usable engine", "err", err)
		return
	}
	release()
}

// Close 实现 tts.Provider：关闭服务并卸载引擎。
func (t *TTSService) Close() error {
	t.slot.Close()
	return nil
}

// IsResident 报告引擎是否驻留内存（测试与诊断用）。
func (t *TTSService) IsResident() bool { return t.slot.IsResident() }
