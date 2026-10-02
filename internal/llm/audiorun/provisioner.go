package audiorun

import (
	"context"
	"log/slog"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
)

// ProvisionerOptions 配置语音资产后台预置器（ADR-0108）。
type ProvisionerOptions struct {
	STT        *STTService                            // 必填
	TTS        *TTSService                            // provider=http 时为 nil
	StartDelay time.Duration                          // 生产 45s
	Backoff    []time.Duration                        // 生产 {5m, 30m, 2h}
	Sink       func(kind string, nextRetry time.Time) // 退避期间发布 next_retry_at，可 nil
	Now        func() time.Time                       // 可注入；nil 用 time.Now

	// 测试可注入函数：若提供则优先使用（满足 §4 要求的以可注入函数实现假 STT/TTS）
	STTInstallFunc func(ctx context.Context) (bool, error)
	TTSInstallFunc func(ctx context.Context) (bool, error)
	TTSBenchFunc   func(ctx context.Context) error
}

// StartProvisioner 在守护进程启动后串行预置语音资产（ADR-0108）。
// 延迟 StartDelay 后按 STT → TTS 顺序串行执行：下载/校验/解压，不常驻引擎。
func StartProvisioner(ctx context.Context, o ProvisionerOptions) {
	concurrent.SafeGo(ctx, "audiorun.provisioner", func(ctx context.Context) {
		runProvisioner(ctx, o)
	})
}

func runProvisioner(ctx context.Context, o ProvisionerOptions) {
	nowFn := o.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	backoffs := o.Backoff
	if len(backoffs) == 0 {
		backoffs = []time.Duration{5 * time.Minute, 30 * time.Minute, 2 * time.Hour}
	}

	if o.StartDelay > 0 {
		select {
		case <-ctx.Done():
			return
		case <-time.After(o.StartDelay):
		}
	}

	slog.Info("audio: provisioner started")

	// 1. 预置 STT
	provisionSTT(ctx, o, backoffs, nowFn)
	if ctx.Err() != nil {
		return
	}

	// 2. 预置 TTS
	provisionTTS(ctx, o, backoffs, nowFn)

	slog.Info("audio: provisioner completed")
}

func provisionSTT(ctx context.Context, o ProvisionerOptions, backoffs []time.Duration, nowFn func() time.Time) {
	sttSupported := false
	var sttFn func(context.Context) (bool, error)
	if o.STTInstallFunc != nil {
		sttSupported = o.STT == nil || o.STT.o.Support.Supported
		sttFn = o.STTInstallFunc
	} else if o.STT != nil {
		sttSupported = o.STT.o.Support.Supported
		sttFn = o.STT.InstallBlocking
	}

	if sttSupported && sttFn != nil {
		if err := executeWithBackoff(ctx, "stt", sttFn, o.Sink, backoffs, nowFn); err != nil {
			slog.Warn("audio: provisioner stt failed", "err", err)
		}
	} else if o.STT != nil && !o.STT.o.Support.Supported {
		slog.Info("audio: provisioner skipping stt (unsupported)", "reason", o.STT.o.Support.Reason)
	}
}

func provisionTTS(ctx context.Context, o ProvisionerOptions, backoffs []time.Duration, nowFn func() time.Time) {
	ttsSupported := false
	ttsTooSlow := false
	var ttsInstallFn func(context.Context) (bool, error)
	var ttsBenchFn func(context.Context) error

	if o.TTSInstallFunc != nil {
		ttsSupported = o.TTS == nil || o.TTS.o.Support.Supported
		ttsTooSlow = o.TTS != nil && o.TTS.slow.Load() != nil
		ttsInstallFn = o.TTSInstallFunc
		ttsBenchFn = o.TTSBenchFunc
	} else if o.TTS != nil {
		ttsSupported = o.TTS.o.Support.Supported
		ttsTooSlow = o.TTS.slow.Load() != nil
		ttsInstallFn = o.TTS.InstallBlocking
		ttsBenchFn = o.TTS.BenchIfNeeded
	}

	if !ttsSupported || ttsTooSlow || ttsInstallFn == nil {
		logSkippedTTS(o)
		return
	}

	err := executeWithBackoff(ctx, "tts", ttsInstallFn, o.Sink, backoffs, nowFn)
	if err == nil && ctx.Err() == nil && ttsBenchFn != nil {
		runTTSBench(ctx, ttsBenchFn)
	}
}

func logSkippedTTS(o ProvisionerOptions) {
	if o.TTS == nil {
		return
	}
	if !o.TTS.o.Support.Supported {
		slog.Info("audio: provisioner skipping tts (unsupported)", "reason", o.TTS.o.Support.Reason)
	} else if o.TTS.slow.Load() != nil {
		slog.Info("audio: provisioner skipping tts (previously judged too slow)")
	}
}

func runTTSBench(ctx context.Context, benchFn func(context.Context) error) {
	slog.Info("audio: provisioner starting bench", "kind", "tts", "step", "bench")
	if berr := benchFn(ctx); berr != nil {
		slog.Warn("audio: provisioner tts bench failed", "kind", "tts", "err", berr)
	} else {
		slog.Info("audio: provisioner finished bench", "kind", "tts", "step", "bench")
	}
}

func executeWithBackoff(
	ctx context.Context,
	kind string,
	installFn func(context.Context) (bool, error),
	sink func(string, time.Time),
	backoffs []time.Duration,
	nowFn func() time.Time,
) error {
	maxAttempts := 1 + len(backoffs)
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if ctx.Err() != nil {
			return apperr.Wrap(apperr.CodeCancelled, "provisioner canceled", ctx.Err())
		}
		slog.Info("audio: provisioner step begin", "kind", kind, "step", "install", "attempt", attempt+1)
		ran, err := installFn(ctx)
		if err == nil {
			slog.Info("audio: provisioner step complete", "kind", kind, "step", "install", "ran", ran)
			if sink != nil {
				sink(kind, time.Time{})
			}
			return nil
		}

		slog.Warn("audio: provisioner step failed", "kind", kind, "step", "install", "attempt", attempt+1, "err", err)
		if attempt < len(backoffs) {
			waitDur := backoffs[attempt]
			nextRetry := nowFn().Add(waitDur)
			if sink != nil {
				sink(kind, nextRetry)
			}
			select {
			case <-ctx.Done():
				return apperr.Wrap(apperr.CodeCancelled, "provisioner canceled", ctx.Err())
			case <-time.After(waitDur):
			}
		}
	}
	slog.Warn("audio: provisioner retries exhausted", "kind", kind)
	return nil
}
