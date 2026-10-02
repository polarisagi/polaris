package probe

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"
)

// AcceleratorInfo 描述本机检测到的算力加速硬件与系统压力（ADR-0109 P3）。
type AcceleratorInfo struct {
	Kind         string  `json:"kind"`          // "apple_silicon" | "nvidia" | "none"
	Model        string  `json:"model"`         // 芯片型号 / GPU 名称
	MemoryMB     uint64  `json:"memory_mb"`     // 统一内存或显存（MB）
	Accelerated  bool    `json:"accelerated"`   // 是否满足加速建议阈值（Apple Silicon>=16GB, NVIDIA>=6GB）
	PhysicalCPUs int     `json:"physical_cpus"` // 物理核数
	Load1Min     float64 `json:"load_1min"`     // 近 1 分钟负载均值
}

// AcceleratorProbe 管理算力硬件的异步探测与结果缓存，防止阻塞服务主流程。
type AcceleratorProbe struct {
	mu   sync.RWMutex
	info AcceleratorInfo
	done bool
}

// NewAcceleratorProbe 创建算力探针实例。
func NewAcceleratorProbe() *AcceleratorProbe {
	return &AcceleratorProbe{
		info: AcceleratorInfo{
			Kind: "none",
		},
	}
}

// Get 返回当前缓存的硬件探测结果。若探测尚未完成，返回兜底默认值。
func (p *AcceleratorProbe) Get() AcceleratorInfo {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.info
}

// Probe 执行平台相关的硬件探测，带 3 秒超时限制，并将结果写入缓存。
func (p *AcceleratorProbe) Probe(ctx context.Context) AcceleratorInfo {
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	info := probePlatformAccelerator(probeCtx)

	p.mu.Lock()
	p.info = info
	p.done = true
	p.mu.Unlock()

	return info
}

// parseNvidiaSmiOutput 解析 nvidia-smi CSV 格式输出。
// 输入格式：`name, memory.total`（例如: "NVIDIA GeForce RTX 3080, 10240"）。
func parseNvidiaSmiOutput(output string) (AcceleratorInfo, error) {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, ",")
		if len(parts) < 2 {
			continue
		}
		name := strings.TrimSpace(parts[0])
		memStr := strings.TrimSpace(parts[1])
		memMB, err := strconv.ParseUint(memStr, 10, 64)
		if err != nil {
			continue
		}
		// 显存 >= 6GB (6144 MB) 视为具备本地大模型加速能力（ADR-0109 P3）
		accelerated := memMB >= 6144
		return AcceleratorInfo{
			Kind:        "nvidia",
			Model:       name,
			MemoryMB:    memMB,
			Accelerated: accelerated,
		}, nil
	}
	return AcceleratorInfo{Kind: "none", Accelerated: false}, nil
}

// parseDarwinDisplaysOutput 解析 system_profiler SPDisplaysDataType 输出中的 GPU 芯片名称。
func parseDarwinDisplaysOutput(output string) string {
	var models []string
	lines := strings.Split(output, "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "Chipset Model:") {
			model := strings.TrimSpace(strings.TrimPrefix(trimmed, "Chipset Model:"))
			if model != "" {
				models = append(models, model)
			}
		}
	}
	if len(models) == 0 {
		return ""
	}
	return strings.Join(models, " / ")
}
