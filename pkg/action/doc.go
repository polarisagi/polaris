// Package action 是 L1 执行层。
// 涵盖模块:
//   - M7 Tool & Action Layer (工具注册、三级沙箱 L1/L2/L3、MCP 双向化、A2A 互操作)
//
// 不变量: [HE-Rule-2] Default-Block + Capability Token 显式授权, [HE-Rule-5] 状态机驱动。
// 依赖: substrate (Storage/Observability/Policy)、cognition (仅被消费接口，无反向依赖)。
package action
