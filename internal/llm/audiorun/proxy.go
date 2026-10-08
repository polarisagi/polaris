package audiorun

import (
	"context"
	"log/slog"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"time"

	"github.com/polarisagi/polaris/internal/llm/tts"
	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// 下载前代理测速（ADR-0110 修订三）。
//
// 为什么要代理：Melo 资产约 167MB，弱 CPU 机器下完才发现 RTF>0.8 等于白下一次。
// SenseVoice（已随 STT 安装，几十 MB 级加载成本）与 Melo 同为 sherpa-onnx/ORT 的 CPU 推理，
// 两者 RTF 随线程数的变化近似同比。M1 实测（同线程数）：
//
//	线程   SenseVoice RTF   Melo RTF   比值
//	 1      0.0391           0.79       20.2
//	 2      0.0223           0.42       18.8
//	 4      0.0172           0.233      13.5
//
// 比值随线程数下降（Melo 并行效率更好）。取 13（最乐观档，即 4 线程比值）：
// 宁可放行一台实际偏慢的机器（后面还有真实基准兜底），也不能因为代理偏悲观而误杀能用的机器。
const (
	// proxyRatio 是 Melo RTF ≈ SenseVoice RTF × proxyRatio 的外推系数。
	proxyRatio = 13.0
	// proxyMaxPredicted 是预测 Melo RTF 的放行上限：> 1.0 意味着连实时都追不上，必然断流。
	// 比真实基准的 MaxTTSRTF(0.8) 宽松，是因为代理有误差，边缘机型交给真实基准定夺。
	proxyMaxPredicted = 1.0

	proxyAudioSeconds = 10
	proxySampleRate   = 16000
	proxyTimedRuns    = 3
	proxyNoiseAmp     = 0.01
	proxyNoiseSeed    = 20261008
)

// ProxyProbeFunc 在 threads 线程下测 SenseVoice 对固定噪声的 RTF。
// ok=false 表示无法测量（STT 资产缺失、内存不足、加载失败等），调用方须跳过代理而不是当作过慢。
type ProxyProbeFunc func(ctx context.Context, threads int) (sttRTF float64, ok bool)

// proxyPredicted 由 SenseVoice RTF 外推 Melo RTF。
func proxyPredicted(sttRTF float64) float64 { return sttRTF * proxyRatio }

// decideProxy 根据代理测速得出结论（纯函数，供表驱动测试）。
// conclusive=false：CPU 争用下的测量不可信，不得据此拒绝下载，走原路径（下载并跑真实基准）。
// conclusive=true 且 rec.Supported=false：确定过慢，不下载。
// 通过（predicted<=1.0）也是 conclusive，但调用方不落库、继续下载。
func decideProxy(sttRTF float64, contended bool) (rec BenchRecord, conclusive bool) {
	if contended {
		return BenchRecord{}, false
	}
	pred := proxyPredicted(sttRTF)
	rec = BenchRecord{
		RTF: pred, Method: MethodProxy, STTRTF: sttRTF, Predicted: pred,
		Supported: pred <= proxyMaxPredicted,
	}
	return rec, true
}

// proxyNoise 生成固定种子的 10 秒 16kHz 低幅噪声：不依赖任何资产文件，且每台机器输入完全一致。
func proxyNoise() []float32 {
	r := rand.New(rand.NewSource(proxyNoiseSeed)) //nolint:gosec // 固定种子的确定性测试信号，非安全用途
	n := proxyAudioSeconds * proxySampleRate
	out := make([]float32, n)
	for i := range out {
		out[i] = (r.Float32()*2 - 1) * proxyNoiseAmp
	}
	return out
}

// MeasureSTTRTF 用 transcribe 对固定噪声计时：1 次预热（丢弃，含 ORT 图优化与内存池建立）
// + proxyTimedRuns 次计时取最小值（排除偶发抢占尖峰）。返回 RTF=耗时/音频时长。
func MeasureSTTRTF(transcribe func(samples []float32, sampleRate int) error) (float64, error) {
	samples := proxyNoise()
	if err := transcribe(samples, proxySampleRate); err != nil {
		return 0, apperr.Wrap(apperr.CodeInternal, "audiorun: 代理测速预热失败", err)
	}
	best := math.MaxFloat64
	for i := 0; i < proxyTimedRuns; i++ {
		start := time.Now()
		if err := transcribe(samples, proxySampleRate); err != nil {
			return 0, apperr.Wrap(apperr.CodeInternal, "audiorun: 代理测速转写失败", err)
		}
		best = math.Min(best, time.Since(start).Seconds()/proxyAudioSeconds)
	}
	return best, nil
}

// proxyTooSlow 在下载 Melo 前运行代理测速。返回 true 表示已定论过慢（已落库、已置 slow、已发布状态），
// 调用方不得下载；false 表示放行（含无结论），按原路径下载并跑真实基准。
func (t *TTSService) proxyTooSlow(ctx context.Context, origin string) bool {
	if t.o.ProxyProbe == nil {
		return false
	}
	usage := t.o.CPUUsage
	if usage == nil {
		usage = func() float64 { return 0 }
	}
	threads := ttsThreads(t.o.Profile.LogicalCores)
	t.pub(Status{State: StateDownloading, Origin: origin, Detail: "下载前评估本机语音合成速度"})
	// 与真实基准同口径（ADR-0108 D2）：后台自动路径先等 CPU 空闲，避免把用户正在跑的负载测成"机器慢"。
	if origin == "auto" && !waitCPUIdle(ctx, usage, benchIdleThreshold, benchIdleSamples, benchIdleInterval, benchIdleMaxWaitAuto) {
		slog.Info("audio: tts proxy bench proceeding without confirmed idle CPU")
	}
	cpuBefore := usage()
	sttRTF, ok := t.o.ProxyProbe(ctx, threads)
	cpuAfter := usage()
	if !ok {
		metrics.GlobalAudioTTSProxySkippedTotal.Add(1)
		slog.Info("audio: tts proxy bench skipped (stt unavailable), falling back to download + real bench")
		return false
	}
	contended := cpuBefore >= benchIdleThreshold || cpuAfter >= benchIdleThreshold
	rec, conclusive := decideProxy(sttRTF, contended)
	if !conclusive {
		metrics.GlobalAudioTTSProxySkippedTotal.Add(1)
		slog.Info("audio: tts proxy bench inconclusive (cpu contended), falling back to download + real bench",
			"stt_rtf", sttRTF, "cpu_before", cpuBefore, "cpu_after", cpuAfter)
		return false
	}
	metrics.GlobalAudioTTSProxyBenchTotal.Add(1)
	slog.Info("audio: tts proxy bench",
		"stt_rtf", sttRTF, "predicted", rec.Predicted, "max_predicted", proxyMaxPredicted,
		"threads", threads, "supported", rec.Supported, "origin", origin)
	if rec.Supported {
		return false
	}
	rec.Fingerprint = t.fingerprint()
	rec.MeasuredAt = time.Now().UTC()
	if serr := SaveBench(ctx, t.o.Prefs, rec); serr != nil {
		slog.Warn("audio: tts proxy bench result not persisted", "err", serr)
	}
	t.slow.Store(&rec)
	metrics.GlobalAudioTTSProxyTooSlowTotal.Add(1)
	slog.Info("audio: tts judged too slow by proxy, skipping Melo download", "predicted", rec.Predicted)
	t.pub(t.base(false))
	return true
}

// reclaimAssets 在真实基准定论过慢后删除 models/tts/melo/，把 167MB 还给磁盘（状态保持 unsupported/too_slow）。
//
// 并发安全：调用点在 Slot 的加载回调内且引擎已 Close —— 此刻 Slot 无驻留引擎、无 inflight；
// 之后任何 Generate 都会被 gate()/loadEngine 对 slow 的检查挡在加载之前，不会再碰这个目录。
// assetMu 再保证与下载/解压互斥。只删目录名恰为 ModelName 的确切路径。
func (t *TTSService) reclaimAssets() {
	dir := tts.ModelDir(t.o.Dir)
	if dir == "" || filepath.Base(dir) != tts.ModelName {
		return
	}
	t.assetMu.Lock()
	defer t.assetMu.Unlock()
	if !dirExists(dir) {
		return
	}
	if err := os.RemoveAll(dir); err != nil {
		slog.Warn("audio: 回收 MeloTTS 资产目录失败（可手动删除）", "dir", dir, "err", err)
		return
	}
	metrics.GlobalAudioTTSReclaimTotal.Add(1)
	slog.Info("audio: 真实基准定论过慢，已回收 MeloTTS 资产目录", "dir", dir)
}

func dirExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
