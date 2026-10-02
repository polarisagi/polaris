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
	// CPUUsage 采样系统 CPU 占用率（0–100）。nil 时视为恒 0（仅测试用）。
	CPUUsage func() float64
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
	// nextLoadOrigin 记录下一次引擎加载的发起方（"auto"|"user"）。
	// 为什么不改 Slot.Load 签名：Slot 是通用的懒加载容器，不应侵入特定引擎的业务来源参数；
	// 通过原子指针在 Acquire 前设置、loadEngine 中读取并复位，保持 Slot 纯粹（HE-3）。
	nextLoadOrigin atomic.Pointer[string]
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
	if ok && !rec.Supported && rec.RetryOnStart {
		// 上次的过慢结论可能是瞬时负载造成：本次启动不采信，用户触发朗读/安装时重测一次。
		slog.Info("audio: tts previously judged too slow, will re-measure once on next use", "rtf", rec.RTF)
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

func (t *TTSService) setLoadOrigin(origin string) {
	o := origin
	t.nextLoadOrigin.Store(&o)
}

func (t *TTSService) takeLoadOrigin() string {
	p := t.nextLoadOrigin.Swap(nil)
	if p != nil && *p != "" {
		return *p
	}
	return "user"
}

// NeedsBench 检查是否需要运行首次基准（资产齐全且无基准记录或处于 RetryOnStart，且无定论 too_slow）。
func (t *TTSService) NeedsBench(ctx context.Context) bool {
	if !t.o.Support.Supported {
		return false
	}
	if t.slow.Load() != nil {
		return false
	}
	if len(tts.MissingAssets(t.o.LibDir, t.o.Dir)) > 0 {
		return false
	}
	rec, ok, err := GetBench(ctx, t.o.Prefs, t.fp)
	if err != nil || !ok {
		return true
	}
	if rec.Supported {
		return false
	}
	return rec.RetryOnStart
}

// BenchIfNeeded 在 NeedsBench 为真时经 slot.AcquireWait 加载运行基准，跑完立即释放并卸载（ADR-0108）。
func (t *TTSService) BenchIfNeeded(ctx context.Context) error {
	if !t.NeedsBench(ctx) {
		return nil
	}
	t.setLoadOrigin("auto")
	_, release, err := t.slot.AcquireWait(ctx, 0)
	if err != nil {
		return err
	}
	release()
	t.slot.Unload()
	return nil
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

	origin := t.takeLoadOrigin()
	if !t.NeedsBench(ctx) {
		return eng, nil
	}
	prev, ok, _ := GetBench(ctx, t.o.Prefs, t.fp)
	return t.firstBench(ctx, eng, ok && !prev.Supported, origin)
}

// firstBench 在首次加载后测 RTF 并持久化；过慢则卸载并置 unsupported(too_slow)。
func (t *TTSService) firstBench(ctx context.Context, eng *tts.Engine, retried bool, origin string) (*tts.Engine, error) {
	t.o.Sink.Publish(Status{State: StateLoading, Origin: origin, Detail: "首次启用：运行语音合成速度基准"})
	usage := t.o.CPUUsage
	if usage == nil {
		usage = func() float64 { return 0 }
	}
	if origin == "auto" {
		if !waitCPUIdle(ctx, usage, benchIdleThreshold, benchIdleSamples, benchIdleInterval, benchIdleMaxWaitAuto) {
			slog.Info("audio: tts bench proceeding without confirmed idle CPU")
		}
	}
	cpuBefore := usage()
	rtf, err := RunBench(ctx, eng)
	cpuAfter := usage()
	if err != nil {
		if cerr := eng.Close(); cerr != nil {
			slog.Warn("audio: tts engine close after bench failure failed", "err", cerr)
		}
		return nil, err
	}
	contended := cpuBefore >= benchIdleThreshold || cpuAfter >= benchIdleThreshold
	rec := decideBench(rtf, contended, retried)
	rec.Fingerprint = t.fp
	rec.MeasuredAt = time.Now().UTC()

	slog.Info("audio: tts benchmark",
		"rtf", rtf, "max_rtf", MaxTTSRTF, "supported", rec.Supported,
		"threads", t.o.NumThreads, "fingerprint", t.fp,
		"cpu_before", cpuBefore, "cpu_after", cpuAfter, "contended", contended, "origin", origin)

	if serr := SaveBench(ctx, t.o.Prefs, rec); serr != nil {
		slog.Warn("audio: tts bench result not persisted", "err", serr)
	}
	if rec.Supported {
		return eng, nil
	}
	if cerr := eng.Close(); cerr != nil {
		slog.Warn("audio: tts engine close after slow bench failed", "err", cerr)
	}
	if !rec.RetryOnStart {
		// 定论：非争用且已重测过仍慢
		t.slow.Store(&rec)
		return nil, notReady(CodeUnsupported, t.tooSlowStatus().Detail)
	}
	// 暂时不可用：本次请求降级系统语音，RetryOnStart=true，不写 t.slow，状态发布为 ready（资产完备，下次空闲重测）
	t.o.Sink.Publish(t.base(false))
	return nil, notReady(CodeUnsupported, "本机服务端语音合成暂时不可用：此刻 CPU 繁忙，朗读暂用系统语音，空闲时会自动重测")
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
	t.setLoadOrigin("user")
	eng, release, err := t.slot.Acquire(ctx)
	if err != nil {
		return tts.Audio{}, err
	}
	defer release()
	return eng.Generate(ctx, text) //nolint:wrapcheck // tts 包已用 apperr 包装
}

func (t *TTSService) installSync(ctx context.Context, origin string) error {
	defer t.installing.Store(false)
	t.o.Sink.Publish(Status{State: StateDownloading, Origin: origin, Detail: "准备下载语音合成模型"})
	if err := tts.EnsureAssets(ctx, t.o.LibDir, t.o.Dir, t.o.HTTPClient, t.o.SherpaVersion, downloadProgress(t.o.Sink, origin)); err != nil {
		slog.Error("audio: tts install failed", "err", err)
		t.o.Sink.Publish(Status{State: StateFailed, Error: err.Error()})
		return apperr.Wrap(apperr.CodeInternal, "tts: ensure assets failed", err)
	}
	slog.Info("audio: tts install complete", "dir", t.o.Dir)
	if origin == "user" {
		t.warm(ctx)
	} else {
		t.o.Sink.Publish(t.base(t.slot.IsResident()))
	}
	return nil
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
	concurrent.SafeGo(t.rootCtx, "audiorun.tts_install", func(ctx context.Context) {
		if err := t.installSync(ctx, "user"); err != nil {
			slog.Warn("audio: tts background install failed", "err", err)
		}
	})
	return true, nil
}

// InstallBlocking 供后台预置器调用（ADR-0108）：
// 不支持 / 已慢判定 / 已安装 / 已在进行中 → ran=false, err=nil；否则同步阻塞执行 installSync(ctx, "auto")。
func (t *TTSService) InstallBlocking(ctx context.Context) (ran bool, err error) {
	if !t.o.Support.Supported || t.slow.Load() != nil || len(tts.MissingAssets(t.o.LibDir, t.o.Dir)) == 0 {
		return false, nil
	}
	if !t.installing.CompareAndSwap(false, true) {
		return false, nil
	}
	return true, t.installSync(ctx, "auto")
}

// warm 安装完成后立即加载一次：既验证资产能真正跑起来，也触发首次基准（结果落库）。
// 失败状态已由 Slot 的 OnLoadFailed 钩子发布，这里只需把终态补齐。
func (t *TTSService) warm(ctx context.Context) {
	t.setLoadOrigin("user")
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
