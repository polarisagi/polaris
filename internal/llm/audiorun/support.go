// Package audiorun 管理语音（STT/TTS）引擎的运行期生命周期：按稳定硬件画像判定是否支持、
// 按需下载、懒加载、空闲卸载、首次启用基准（ADR-0107）。
//
// 它把 stt/tts 两个"只会加载与推理"的包包成可被 HTTP 层安全调用的服务：
// 状态变化经 StatusSink 汇报给调用方（调用方在 cmd 层把它接到前端可见的状态机）。
package audiorun

import (
	"fmt"
	"runtime"
	"strings"

	"github.com/polarisagi/polaris/internal/llm/audioassets"
)

// 硬件两档阈值（ADR-0110 2026-10-08 修订；STT 门槛沿用 ADR-0107）。
//
// 为什么名义 2GB 的总内存阈值要留余量：2GB 的 VPS 在 Linux 下 MemTotal 实测只有
// ≈1.9GB（内核/固件保留），按字面 2048MB 判定会把标称 2GB 的机器全部误判为不支持，
// 而这正是语音要覆盖的目标机型。取名义值的 ≈88%，仍能挡住 1GB 机器。
//
//	A 极低配：总内存 < 1800MB 或逻辑核 < 2 → STT/TTS 都不开，不预置任何资产
//	支持档：  其余 → STT + MeloTTS；Melo 能否实际使用由抗争用基准（RTF ≤ 0.8）决定，
//	          过慢则回退前端系统语音（没有更轻的服务端模型：Matcha 因训练数据来源不可核验已删除，见 ADR-0110 修订）
const (
	sttMinTotalMB = 1800
	sttMinCores   = 2

	// 加载时空闲内存门槛：只决定"此刻能否加载"，不参与"是否支持"的判定。
	// 各取单句 RSS 实测值的约 1.5 倍余量：STT≈420MB、Melo≈530MB。
	// 2GB 机器上两者不能同驻时，由门槛拒绝后加载者（503 契约），而不是靠降档规避。
	sttMinFreeMB = 600
	ttsMinFreeMB = 800

	// MaxTTSRTF 是服务端 TTS 可接受的最大实时率（合成耗时/音频时长）。
	// 前端按句预取、边播边合成，只要 RTF<1 就不会断流，0.8 在此基础上留 20% 余量；
	// 原先的 0.7 过严：空闲 i9-9880H 上实测 fp32 TTS 为 0.65，几乎贴线，
	// 稍有后台负载就会被误判（嵌入 runner 抢占 CPU 时实测 1.07）。
	MaxTTSRTF = 0.8
)

// ttsThreads 返回 MeloTTS 推理线程数 min(4, 逻辑核)，至少 1（ADR-0110）。
// 不随内存档位走：内存大不代表核多，16GB 的 2 核云主机开 4 线程只会互相抢核。
func ttsThreads(cores int) int {
	if cores < 1 {
		return 1
	}
	if cores > 4 {
		return 4
	}
	return cores
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

// Support 汇总 STT 与 TTS 两项能力。两者门槛相同（A 档同关），TTS 另需基准通过。
type Support struct {
	STT, TTS Capability
}

// AudioSupport 按稳定硬件画像判定 STT/TTS 是否支持。
func AudioSupport(p HardwareProfile) Support {
	c := judge(p)
	if c.Supported {
		return Support{STT: c, TTS: c}
	}
	// 文案按能力点名，其余字段（原因码）共用。
	stt, tts := c, c
	stt.Message = strings.Replace(c.Message, "语音功能", "语音输入", 1)
	tts.Message = strings.Replace(c.Message, "语音功能", "服务端朗读", 1)
	return Support{STT: stt, TTS: tts}
}

func judge(p HardwareProfile) Capability {
	if _, ok := audioassets.LibAsset(p.GOOS, p.GOARCH); !ok {
		return Capability{Reason: ReasonUnsupportedPlatform, Message: fmt.Sprintf(
			"语音功能不支持当前平台 %s/%s（无 sherpa-onnx 预编译库）", p.GOOS, p.GOARCH)}
	}
	if p.totalMB() < sttMinTotalMB {
		return Capability{Reason: ReasonInsufficientRAM, Message: fmt.Sprintf(
			"语音功能需要至少 2GB 内存 / 2 核（本机总内存 %dMB）", p.totalMB())}
	}
	if p.LogicalCores < sttMinCores {
		return Capability{Reason: ReasonInsufficientCores, Message: fmt.Sprintf(
			"语音功能需要至少 2GB 内存 / 2 核（本机 %d 个逻辑核）", p.LogicalCores)}
	}
	return Capability{Supported: true}
}
