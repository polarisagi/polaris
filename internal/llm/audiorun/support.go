// Package audiorun 管理语音（STT/TTS）引擎的运行期生命周期：按稳定硬件画像判定是否支持、
// 按需下载、懒加载、空闲卸载、首次启用基准（ADR-0107）。
//
// 它把 stt/tts 两个"只会加载与推理"的包包成可被 HTTP 层安全调用的服务：
// 状态变化经 StatusSink 汇报给调用方（调用方在 cmd 层把它接到前端可见的状态机）。
package audiorun

import (
	"fmt"
	"runtime"

	"github.com/polarisagi/polaris/internal/llm/audioassets"
	"github.com/polarisagi/polaris/internal/llm/tts"
)

// 硬件三档阈值（ADR-0110 决策 2；STT 门槛沿用 ADR-0107）。
//
// 为什么名义 2GB/4GB 的总内存阈值要留余量：2GB 的 VPS 在 Linux 下 MemTotal 实测只有
// ≈1.9GB（内核/固件保留），按字面 2048MB 判定会把标称 2GB 的机器全部误判为不支持，
// 而这正是 STT 要覆盖的目标机型。取名义值的 ≈88%，仍能挡住 1GB 机器。
//
//	A 极低配：总内存 < 1800MB 或逻辑核 < 2  → STT/TTS 都不开，不预置任何资产
//	B 低配：  不属 A，且（总内存 < 3600MB 或逻辑核 < 4）→ STT + Matcha
//	C 标准：  总内存 ≥ 3600MB 且逻辑核 ≥ 4 → STT + MeloTTS
const (
	sttMinTotalMB = 1800
	sttMinCores   = 2
	ttsMinTotalMB = 3600 // C 档（Melo）门槛
	ttsMinCores   = 4

	// 加载时空闲内存门槛：只决定"此刻能否加载"，不参与"是否支持"的判定。
	// 各取单句 RSS 实测值的约 1.5 倍余量：STT≈420MB、Melo≈530MB、Matcha≈280MB。
	sttMinFreeMB    = 600
	meloMinFreeMB   = 800
	matchaMinFreeMB = 450

	// MaxTTSRTF 是服务端 TTS 可接受的最大实时率（合成耗时/音频时长）。
	// 前端按句预取、边播边合成，只要 RTF<1 就不会断流，0.8 在此基础上留 20% 余量；
	// 原先的 0.7 过严：空闲 i9-9880H 上实测 Kokoro fp32 为 0.65，几乎贴线，
	// 稍有后台负载就会被误判（嵌入 runner 抢占 CPU 时实测 1.07）。
	MaxTTSRTF = 0.8
)

// TTSModelNone 表示本机不开服务端 TTS（A 档 / 平台不支持）。
const TTSModelNone = "none"

// 档位标识。
const (
	TierA = "A"
	TierB = "B"
	TierC = "C"
)

// MinFreeMB 返回加载某 TTS 模型所需的空闲内存门槛。
func MinFreeMB(m tts.Model) uint64 {
	if m == tts.ModelMatcha {
		return matchaMinFreeMB
	}
	return meloMinFreeMB
}

// ThreadsFor 返回 TTS 推理线程数：Melo min(4, 核)、Matcha min(2, 核)，至少 1（ADR-0110 决策 6）。
// 不随内存档位走：内存大不代表核多，16GB 的 2 核云主机开 4 线程只会互相抢核。
func ThreadsFor(m tts.Model, cores int) int {
	limit := 4
	if m == tts.ModelMatcha {
		limit = 2
	}
	if cores < 1 {
		cores = 1
	}
	if cores < limit {
		return cores
	}
	return limit
}

// 不支持原因码（Capability.Reason）。
const (
	ReasonUnsupportedPlatform = "unsupported_platform"
	ReasonInsufficientRAM     = "insufficient_ram"
	ReasonInsufficientCores   = "insufficient_cores"
	ReasonTooSlow             = "too_slow"
)

// HardwareProfile 是判定"是否支持"所用的稳定硬件画像。
// 刻意不含空闲内存：瞬时值会让同一台机器时而支持时而不支持，状态在前端来回跳。
type HardwareProfile struct {
	TotalRAMBytes uint64
	LogicalCores  int
	GOOS, GOARCH  string
}

// HostProfile 用已采集的总内存与运行时信息构造本机画像。
func HostProfile(totalRAMBytes uint64) HardwareProfile {
	return HardwareProfile{
		TotalRAMBytes: totalRAMBytes,
		LogicalCores:  runtime.NumCPU(),
		GOOS:          runtime.GOOS,
		GOARCH:        runtime.GOARCH,
	}
}

func (p HardwareProfile) totalMB() uint64 { return p.TotalRAMBytes / (1024 * 1024) }

// Capability 是某项语音能力在本机上是否被支持的静态判定。
type Capability struct {
	Supported bool
	Reason    string // Supported=false 时的原因码
	Message   string // Supported=false 时面向用户的说明（含最低配置）
}

// Support 汇总 STT 与 TTS 两项能力，以及选中的 TTS 模型（ADR-0110）。
type Support struct {
	STT, TTS Capability
	// Tier 是硬件档位（TierA/TierB/TierC）；平台无预编译库时为空。
	Tier string
	// TTSModel 是选中的服务端 TTS 模型："melo" | "matcha" | TTSModelNone。
	TTSModel string
	// TTSNote 是面向用户的档位说明（含 TTS 不可用时的原因），任何档位都填写。
	TTSNote string
}

// Model 返回选中的 TTS 模型；TTSModelNone 时 ok=false。
func (s Support) Model() (tts.Model, bool) {
	if s.TTSModel == string(tts.ModelMelo) || s.TTSModel == string(tts.ModelMatcha) {
		return tts.Model(s.TTSModel), true
	}
	return "", false
}

// AudioSupport 按稳定硬件画像判定 STT/TTS 是否支持，并选出 TTS 模型。
//
// ttsPref 是 inference.tts.model：""/"auto" 按档位选；"melo"/"matcha" 显式指定，
// 可越档（B 档机器指定 melo 允许，仍受空闲内存门槛与基准门控约束），但不能解锁 A 档。
// 未知取值按 auto 处理（配置层已 Fail-Fast 校验，这里不再报错）。
func AudioSupport(p HardwareProfile, ttsPref string) Support {
	none := func(c Capability) Support {
		return Support{STT: c, TTS: c, TTSModel: TTSModelNone, TTSNote: c.Message}
	}
	if _, ok := audioassets.LibAsset(p.GOOS, p.GOARCH); !ok {
		c := Capability{Reason: ReasonUnsupportedPlatform, Message: fmt.Sprintf(
			"语音功能不支持当前平台 %s/%s（无 sherpa-onnx 预编译库）", p.GOOS, p.GOARCH)}
		return none(c)
	}
	// A 档：连 STT 都跑不动。两项都关，前端用系统语音。
	if p.totalMB() < sttMinTotalMB || p.LogicalCores < sttMinCores {
		reason, detail := ReasonInsufficientRAM, fmt.Sprintf("本机总内存 %dMB", p.totalMB())
		if p.totalMB() >= sttMinTotalMB {
			reason, detail = ReasonInsufficientCores, fmt.Sprintf("本机 %d 个逻辑核", p.LogicalCores)
		}
		in := func(what string) Capability {
			return Capability{Reason: reason, Message: fmt.Sprintf("%s需要至少 2GB 内存 / 2 核（%s）", what, detail)}
		}
		s := Support{STT: in("语音输入"), TTS: in("服务端朗读"), Tier: TierA, TTSModel: TTSModelNone}
		s.TTSNote = s.TTS.Message + "，朗读使用系统语音"
		return s
	}

	tier := TierC
	if p.totalMB() < ttsMinTotalMB || p.LogicalCores < ttsMinCores {
		tier = TierB
	}
	model := tts.ModelMelo
	if tier == TierB {
		model = tts.ModelMatcha
	}
	note := ""
	switch tts.Model(ttsPref) {
	case tts.ModelMelo, tts.ModelMatcha:
		if m := tts.Model(ttsPref); m != model {
			note = fmt.Sprintf("按配置 inference.tts.model=%s 使用，高于/低于本机 %s 档默认的 %s", m, tier, model)
			model = m
		}
	}
	if note == "" {
		if tier == TierB {
			note = fmt.Sprintf("本机配置较低（总内存 %dMB / %d 核），服务端朗读使用轻量模型 Matcha", p.totalMB(), p.LogicalCores)
		} else {
			note = "服务端朗读使用 MeloTTS"
		}
	}
	return Support{
		STT:      Capability{Supported: true},
		TTS:      Capability{Supported: true},
		Tier:     tier,
		TTSModel: string(model),
		TTSNote:  note,
	}
}
