package audiorun

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/polarisagi/polaris/internal/llm/tts"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// BenchPrefKey 是首次基准结果在 preferences 表里的键（HE-6：状态必须落库，不能只放内存，
// 否则每次重启都要重跑基准，且 unsupported 的判定会在重启后丢失）。
const BenchPrefKey = "audio.tts_bench"

// BenchSentence 是首次基准使用的固定句（audio-v2-spec §2.4）：含标点、数字与日期，
// 覆盖 rule_fsts 路径，使 RTF 反映真实朗读负载而非最轻的短句。
const BenchSentence = "你好，这是一次语音合成速度测试，今天是二零二六年十月二日。"

// PrefStore 是基准结果的持久化接口（调用方定义；*repo.SQLiteSystemRepository 直接满足）。
// 键不存在时 GetPreference 返回 ("", nil)。
type PrefStore interface {
	GetPreference(ctx context.Context, key string) (string, error)
	UpsertPreference(ctx context.Context, key, value string) error
}

const (
	benchIdleThreshold   = 50.0
	benchIdleSamples     = 3
	benchIdleInterval    = 2 * time.Second
	benchIdleMaxWaitAuto = 10 * time.Minute
)

// BenchRecord 是一次基准的持久化结果。
type BenchRecord struct {
	Fingerprint string    `json:"fingerprint"`
	RTF         float64   `json:"rtf"`
	Supported   bool      `json:"supported"`
	MeasuredAt  time.Time `json:"measured_at"`
	// Contended 记录基准开始或结束时 CPU 是否 ≥ 50%（ADR-0108）。
	Contended bool `json:"contended,omitempty"`
	// RetryOnStart 仅在 unsupported 结论上为 true：该结论可能来自瞬时 CPU 争抢（实测 0.65 vs 1.07），
	// 下次守护进程启动后不直接采信，等用户再次触发朗读/安装时重测一次；
	// 重测仍过慢则写入 false，此后同指纹不再重测。supported 结论同指纹始终复用。
	RetryOnStart bool `json:"retry_on_start,omitempty"`
}

// decideBench 根据 RTF、CPU 争用标志以及是否已重测计算基准判定结论（纯函数，供表驱动测试）。
func decideBench(rtf float64, contended, retried bool) BenchRecord {
	if rtf <= MaxTTSRTF {
		return BenchRecord{
			RTF:          rtf,
			Contended:    contended,
			Supported:    true,
			RetryOnStart: false,
		}
	}
	if contended {
		return BenchRecord{
			RTF:          rtf,
			Contended:    contended,
			Supported:    false,
			RetryOnStart: true,
		}
	}
	if !retried {
		return BenchRecord{
			RTF:          rtf,
			Contended:    contended,
			Supported:    false,
			RetryOnStart: true,
		}
	}
	return BenchRecord{
		RTF:          rtf,
		Contended:    contended,
		Supported:    false,
		RetryOnStart: false,
	}
}

// waitCPUIdle 等待 CPU 连续 need 次采样低于 threshold。
// 若在 maxWait 内满足，返回 true；若超时返回 false。
// ctx 取消时立即返回 false。
func waitCPUIdle(ctx context.Context, usage func() float64, threshold float64, need int, interval, maxWait time.Duration) bool {
	if usage == nil || need <= 0 {
		return true
	}
	consecutive := 0
	if usage() < threshold {
		consecutive++
		if consecutive >= need {
			return true
		}
	} else {
		consecutive = 0
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var deadline <-chan time.Time
	if maxWait > 0 {
		timer := time.NewTimer(maxWait)
		defer timer.Stop()
		deadline = timer.C
	}

	for {
		select {
		case <-ctx.Done():
			return false
		case <-deadline:
			return false
		case <-ticker.C:
			if usage() < threshold {
				consecutive++
				if consecutive >= need {
					return true
				}
			} else {
				consecutive = 0
			}
		}
	}
}

// Fingerprint 返回硬件指纹（arch + 逻辑核 + 总内存 GiB 取整）。
// 指纹变化（换机器、改 VM 配置）才重测；总内存取整到 GiB，避免 VM 内存统计的微小抖动误触发重测。
func Fingerprint(p HardwareProfile) string {
	gib := (p.TotalRAMBytes + (1 << 29)) >> 30
	return fmt.Sprintf("%s/%s;cores=%d;ram_gib=%d", p.GOOS, p.GOARCH, p.LogicalCores, gib)
}

// GetBench 读取已持久化的基准；记录不存在、损坏或指纹不匹配都视为"需要重测"（ok=false）。
// 读取/解析失败不致命（重测即可），但必须留痕而不是静默。
func GetBench(ctx context.Context, store PrefStore, fingerprint string) (BenchRecord, bool, error) {
	if store == nil {
		return BenchRecord{}, false, nil
	}
	raw, err := store.GetPreference(ctx, BenchPrefKey)
	if err != nil {
		return BenchRecord{}, false, apperr.Wrap(apperr.CodeInternal, "audiorun: 读取 TTS 基准失败", err)
	}
	if raw == "" {
		return BenchRecord{}, false, nil
	}
	var rec BenchRecord
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		return BenchRecord{}, false, apperr.Wrap(apperr.CodeInternal, "audiorun: TTS 基准记录已损坏，将重测", err)
	}
	if rec.Fingerprint != fingerprint {
		return BenchRecord{}, false, nil // 硬件变了：旧结论作废
	}
	return rec, true, nil
}

// SaveBench 持久化基准结果。
func SaveBench(ctx context.Context, store PrefStore, rec BenchRecord) error {
	if store == nil {
		return nil
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "audiorun: 序列化 TTS 基准失败", err)
	}
	if err := store.UpsertPreference(ctx, BenchPrefKey, string(b)); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "audiorun: 持久化 TTS 基准失败", err)
	}
	return nil
}

// benchTimedRuns 是计时合成次数；取最小 RTF，排除偶发的 CPU 抢占尖峰（最小值最接近硬件真实能力）。
const benchTimedRuns = 2

// RunBench 用固定句合成并返回 RTF = 合成耗时 / 音频时长（计时 benchTimedRuns 次取最小）。
// 先用同一句预热一次丢弃：首次推理含 ORT 图优化与内存池建立，计入会把 RTF 系统性高估，
// 让本来够快的机器被误判为 too_slow。
func RunBench(ctx context.Context, p tts.Provider) (float64, error) {
	if _, err := p.Generate(ctx, BenchSentence); err != nil {
		return 0, apperr.Wrap(apperr.CodeInternal, "audiorun: TTS 基准预热失败", err)
	}
	best := math.MaxFloat64
	for i := 0; i < benchTimedRuns; i++ {
		start := time.Now()
		a, err := p.Generate(ctx, BenchSentence)
		elapsed := time.Since(start)
		if err != nil {
			return 0, apperr.Wrap(apperr.CodeInternal, "audiorun: TTS 基准合成失败", err)
		}
		if a.Duration <= 0 {
			return 0, apperr.New(apperr.CodeInternal, "audiorun: TTS 基准未得到音频时长，无法计算 RTF")
		}
		best = math.Min(best, elapsed.Seconds()/a.Duration.Seconds())
	}
	return best, nil
}
