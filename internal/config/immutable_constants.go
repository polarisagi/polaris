// Package config 编译期不可变常量（L4 不可变内核）。
// 修改此文件需通过 M9 L4 白名单审批流程 + CI immutable_kernel_check 扫描。
// 架构文档: docs/arch/09-Self-Improvement-Engine-深度选型.md §3,
//           docs/arch/11-Policy-Safety-深度选型.md §1

package config

// Layer 1 — 不可侵犯条款（编译期常量）。
// 以下常量若移除或置 false → 编译/测试失败。
const (
	AuditLogAlwaysOn      = true // 审计日志永远开启
	SelfModificationGuard = true // 自修改保护
)

// KillSwitch 端点（不可变）。
const KillSwitchEndpoint = "/_admin/kill"

// HITL auto_approve 硬编码约束。
// 禁止白名单: write_network, privileged, delete_data, execute_system, modify_policy
// 允许白名单: read_local_file, log_rotate, cache_evict, stats_collect
//
// 使用访问器而非 var，防止恶意扩展在运行期覆写此列表绕过内核安全隔离。
func AutoApproveAllowedActions() []string {
	return []string{
		"read_local_file",
		"log_rotate",
		"cache_evict",
		"stats_collect",
	}
}

// L4 不可变内核包（CI merge-block + pre-receive hook 三重保护）。
// 白名单: pkg/swarm/**, pkg/cognition/skill/**, pkg/cognition/memory/**, pkg/edge/**
// 其他包全部禁止 L4 修改。
//
// 使用访问器而非 var，防止恶意扩展在运行期覆写此列表绕过内核安全隔离。
func ImmutableKernelPackages() []string {
	return []string{
		"pkg/substrate/policy/",
		"pkg/substrate/observability/",
		"pkg/cognition/kernel/",
		"pkg/action/",
		"internal/config/",
	}
}
