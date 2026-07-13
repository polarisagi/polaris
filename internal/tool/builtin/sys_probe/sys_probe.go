package sys_probe

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/polarisagi/polaris/internal/sysinfo"
	"github.com/polarisagi/polaris/internal/tool/sandbox"
)

// SysProbeFn 输出结构：sysinfo 字段展平在顶层（保持既有调用方/LLM 已见过的形状不变），
// sandbox_probe 为可选新增字段（Rust dylib 不可用时静默缺省，不影响本工具其余输出）。
type sysProbeOutput struct {
	*sysinfo.SystemInfo
	SandboxProbe    map[string]any `json:"sandbox_probe,omitempty"`
	WasmtimeHealthy bool           `json:"wasmtime_healthy"`
}

func SysProbeFn(_ context.Context, _ []byte) ([]byte, error) {
	out := sysProbeOutput{SystemInfo: sysinfo.GetSystemInfo()}

	// RustSandboxProbeTools 探测沙箱能力 + 已安装语言运行时（2026-07-13 deadcode
	// 复核发现该函数实现完整但从未被本工具调用，与其自身文档"供 sys_probe 工具...
	// 使用"不符）。Tier0 无 Rust dylib 场景下返回 error 是预期行为，静默降级为
	// 缺省字段，不影响本工具其余输出（sandbox 包自身对 exec/wrap_argv 走
	// fail-closed，但诊断信息缺失不应阻断整个探针工具）。
	if probe, err := sandbox.RustSandboxProbeTools(); err == nil {
		out.SandboxProbe = probe
	} else {
		slog.Debug("sys_probe: rust sandbox probe unavailable, skipping", "err", err)
	}

	// WasmtimePing 同样文档自称供诊断使用但此前零调用点；健康检查失败不影响
	// 本工具其余输出（Wasmtime 沙箱本身已有"失败在 WasmtimeExecute 时才报错拦截"
	// 的惰性设计，这里只是把该状态暴露给诊断工具，不改变惰性容错行为）。
	out.WasmtimeHealthy = sandbox.WasmtimePing() == nil

	return json.Marshal(out) //nolint:wrapcheck
}
