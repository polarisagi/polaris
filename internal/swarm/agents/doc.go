// Package agents 提供 Polaris 多 Agent 系统中的常驻后台 Agent 角色实现。
//
// 角色定义：
//   - MemoryAgent (记忆管家)：L1→L2 蒸馏 + 耳语推送 + Extension Librarian 调度。
//   - GovernanceAgent (治理守门人)：PolicyGate 包装 + 幂等网关 + 内存压力监控。
//
// 与 Orchestrator 的关系：
//
//	本包中的 Agent 均为常驻 goroutine，通过 channel 与主脑通信，
//	不经过 Orchestrator 的 tasks 表，不消耗 Orchestrator maxAgents slot。
//
// Tier-0 约束：
//
//	两个 Agent 静止时内存开销 < 5MB（goroutine stack + 结构体）。
//	LLM 调用频率由 distillInterval 和 probeInterval 控制。
package agents
