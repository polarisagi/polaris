// Package cognition 是 L1 认知核心层。
// 涵盖模块:
//   - M4 Agent Kernel (7 状态 FSM、DAG 执行器、System 1/2 双轨路由)
//   - M5 Memory System (四层记忆 L0 Working/L1 Episodic/L2 Semantic/L3 Procedural)
//   - M6 Skill Library (SKILL.md 表征、Logic Collapse 轨迹→Wasm 编译)
//
// 不变量: [HE-Rule-5] 状态机持有控制流, [HE-Rule-3] 可组合原语。
// 依赖: substrate (Inference/Storage/Observability)、action (ToolRegistry 接口)。
package cognition
