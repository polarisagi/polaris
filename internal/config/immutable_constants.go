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
// 白名单见下方 return 列表（本注释此前长期与实际返回值不一致，误写"pkg/swarm/**,
// pkg/cognition/skill/**, pkg/cognition/memory/**, pkg/edge/**"——2026-07-12
// 排查 internal/execute 迁移影响面时发现，一并修正）。
// 其他包全部禁止 L4 修改。
//
// 使用访问器而非 var，防止恶意扩展在运行期覆写此列表绕过内核安全隔离。
func ImmutableKernelPackages() []string {
	return []string{
		"internal/security/",
		"internal/observability/",
		"internal/agent/",
		"internal/action/",
		"internal/config/",
		// internal/execute/dag/ 承载 S_VALIDATE 四层校验（L1 TaintGate + L1 Cedar
		// PolicyGate，HE-2 可验证执行边界）。2026-07-12 前物理位于 internal/agent/dag/，
		// 落在 "internal/agent/" 前缀内自动获得保护；随 internal/execute 模块化迁出
		// 后不再匹配任何既有前缀，若不显式补充会静默失去 L4 保护——故单独列出，
		// 不放开整个 internal/execute/（orchestrator 等其余子包此前从未受保护，
		// 迁移不应顺带扩大保护范围）。
		"internal/execute/dag/",
		"pkg/",
	}
}
