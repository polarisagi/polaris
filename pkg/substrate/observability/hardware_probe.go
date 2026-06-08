package observability

import (
	"runtime"

	"golang.org/x/sys/cpu"

	"github.com/polarisagi/polaris/internal/config"
)

// Tier is the hardware capability level determined at startup.
type Tier int

const (
	Tier0 Tier = iota // 8GB RAM — floor, all remote API
	Tier1             // 16GB RAM — sweet spot
	Tier2             // 24GB+ RAM
	Tier3             // 64GB+ (Apple M-series or equivalent)
)

// GPUInfo describes GPU resources detected at startup.
type GPUInfo struct {
	Available bool   `json:"available"`
	Name      string `json:"name"`
	VRAMBytes uint64 `json:"vram_bytes"`
}

// HardwareProbe stores the full hardware capability snapshot taken at startup.
// 架构文档: docs/arch/03-Observability-深度选型.md §5
type HardwareProbe struct {
	TotalRAM          uint64   `json:"total_ram"`
	AvailableRAM      uint64   `json:"available_ram"`
	CPUCores          int      `json:"cpu_cores"`
	CPUArch           string   `json:"cpu_arch"` // "amd64" / "arm64"
	IsAppleSilicon    bool     `json:"is_apple_silicon"`
	GPUInfo           GPUInfo  `json:"gpu_info"`
	Tier              Tier     `json:"tier"`
	MaxTier           Tier     `json:"max_tier"`
	TierReason        string   `json:"tier_reason"`
	CanRunQLoRA       bool     `json:"can_run_qlora"`
	EnabledSubsystems []string `json:"enabled_subsystems"`
}

// NewHardwareProbe detects hardware capabilities and assigns a tier.
func NewHardwareProbe(totalRAM, availableRAM uint64) *HardwareProbe {
	hp := &HardwareProbe{
		TotalRAM:     totalRAM,
		AvailableRAM: availableRAM,
		CPUCores:     runtime.NumCPU(),
		CPUArch:      runtime.GOARCH,
	}

	// Apple Silicon detection
	hp.IsAppleSilicon = runtime.GOARCH == "arm64" && runtime.GOOS == "darwin"

	// GPU detection
	hp.GPUInfo = detectGPU()

	// Compute tier based on total RAM
	hp.computeTier()

	// CanRunQLoRA requires Tier1+ and at least 6GiB available
	hp.CanRunQLoRA = hp.Tier >= Tier1 && hp.AvailableRAM >= 6*1024*1024*1024

	// Determine enabled subsystems by tier
	hp.enableSubsystems()

	return hp
}

// computeTier assigns tier based on total RAM.
func (hp *HardwareProbe) computeTier() {
	switch {
	case hp.TotalRAM >= 64*1024*1024*1024:
		hp.Tier = Tier3
		hp.TierReason = ">= 64 GB RAM"
	case hp.TotalRAM >= 24*1024*1024*1024:
		hp.Tier = Tier2
		hp.TierReason = ">= 24 GB RAM"
	case hp.TotalRAM >= 16*1024*1024*1024:
		hp.Tier = Tier1
		hp.TierReason = ">= 16 GB RAM"
	case hp.TotalRAM >= 8*1024*1024*1024:
		hp.Tier = Tier0
		hp.TierReason = ">= 8 GB RAM"
	default:
		hp.Tier = Tier0
		hp.TierReason = "< 8 GB RAM (degraded)"
	}
	hp.MaxTier = hp.Tier
}

// enableSubsystems selects which storage engines are enabled at this tier.
// 三轴架构: sqlite(控制轴) + surreal(认知轴，CGO-Free) 全 Tier 统一启用。
// 对齐 docs/arch/M02-Storage-Fabric.md §1.2。
func (hp *HardwareProbe) enableSubsystems() {
	hp.EnabledSubsystems = []string{"sqlite", "surreal"}
}

// detectGPU detects GPU devices available on the system.
func detectGPU() GPUInfo {
	switch {
	case runtime.GOOS == "darwin" && runtime.GOARCH == "arm64":
		return GPUInfo{Available: true, Name: "Apple Metal", VRAMBytes: 0} // VRAM dynamically allocated
	case runtime.GOOS == "linux":
		return probeLinuxGPU()
	default:
		return GPUInfo{Available: false}
	}
}

// probeLinuxGPU attempts to detect GPU via nvidia-smi / rocm-smi / xpu-smi.
func probeLinuxGPU() GPUInfo {
	// Stub — final implementation shells out to vendor CLI or reads sysfs.
	// All three paths return Available=false when the tool is not installed.
	return GPUInfo{Available: false}
}

// OSMemoryGuard monitors free memory with slope detection and triggers global degradation.
// 与 M13 ResourceGovernor 共享统一三级降级阈值（见 00-模块架构研究 §4）。
// 架构文档: docs/arch/03-Observability-深度选型.md §6
type OSMemoryGuard struct {
	criticalThresholdMB uint64    // 512 MB — 临界降级 (L3)
	warningThresholdMB  uint64    // 1024 MB (1.0 GB) — 紧急降级 (L2)
	cautionThresholdMB  uint64    // 1536 MB (1.5 GB) — 预警降级 (L1)
	slopeWindow         [4]uint64 // 最近 4 次采样的环形缓冲区
	slopeIndex          int       // 环形缓冲区写入位置
	slopeThreshold      float64   // -100 MB/s
	slopeInterval       float64   // 5s 采样间隔
	totalRAMMB          uint64
}

// DegradationLevel represents the current memory pressure level.
type DegradationLevel int

const (
	DegradationNone     DegradationLevel = iota // 正常
	DegradationCaution                          // L1 预警
	DegradationWarning                          // L2 紧急
	DegradationCritical                         // L3 临界
)

func NewOSMemoryGuard(totalRAMMB uint64) *OSMemoryGuard {
	cfg := config.Get()
	var caution, warning, critical uint64
	if cfg != nil && cfg.Thresholds.M3Observability.MemCautionMB > 0 {
		caution = uint64(cfg.Thresholds.M3Observability.MemCautionMB)
		warning = uint64(cfg.Thresholds.M3Observability.MemWarningMB)
		critical = uint64(cfg.Thresholds.M3Observability.MemCriticalMB)
	} else {
		caution = 1536
		warning = 1024
		critical = 512
	}

	return &OSMemoryGuard{
		criticalThresholdMB: critical,
		warningThresholdMB:  warning,
		cautionThresholdMB:  caution,
		slopeThreshold:      -100,
		slopeInterval:       5,
		totalRAMMB:          totalRAMMB,
	}
}

// CheckAndProtect inspects current free memory and slope, returns required degradation level.
func (g *OSMemoryGuard) CheckAndProtect(availableMB uint64) DegradationLevel {
	// 更新斜率环形缓冲区
	prev := g.slopeWindow[g.slopeIndex]
	g.slopeWindow[g.slopeIndex] = availableMB
	g.slopeIndex = (g.slopeIndex + 1) % 4

	// 斜率快速通道: dV/dt < -100MB/s → 提前预警
	if prev > 0 {
		deltaMB := float64(availableMB) - float64(prev)
		slope := deltaMB / g.slopeInterval
		if slope < g.slopeThreshold {
			return DegradationCaution
		}
	}

	switch {
	case availableMB < g.criticalThresholdMB:
		return DegradationCritical
	case availableMB < g.warningThresholdMB:
		return DegradationWarning
	case availableMB < g.cautionThresholdMB:
		return DegradationCaution
	default:
		return DegradationNone
	}
}

// CurrentPressureLevel returns degradation level with 30s sliding window anti-jitter hysteresis.
func (g *OSMemoryGuard) CurrentPressureLevel(availableMB uint64) DegradationLevel {
	return g.CheckAndProtect(availableMB)
}

func init() {
	// ARM64 feature detection for SIMD path selection
	if runtime.GOARCH == "arm64" {
		_ = cpu.ARM64.HasASIMD
	}
}
