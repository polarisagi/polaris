package lam

// ContinuousAction 用于 LAM (Large Action Model) 和 Diffusion Policy 的连续动作表示。
// 架构文档: docs/arch/07-Tool-Action-Layer-深度选型.md §7.2
// MVP 不实现 vision 解析路径，Computer Use 仅文本+坐标动作。vision 解析 Tier 1+ 研究分支。
type ContinuousAction struct {
	ActionType   string    // "tool_call" | "mouse_delta" | "key_sequence"
	ActionVector []float64 // 连续动作向量
	Horizon      int       // 预测时间步数
	Confidence   float64   // 0-1 置信度
}

// 2026-07-14（ADR-0051）：ActionDiscretizer/Discretize/ActionProjector/
// keyToCentroid/cosineSim/normalizeVec 删除——全仓零生产调用点。StreamingActionBus
// 是 ContinuousAction 唯一生产消费方，直接将其转发给 DisplayServer.SendAction
// （GUI 连续动作，不需要离散化），从未有调用方构造 ActionDiscretizer 并调用
// Discretize() 把 LAM 向量路由到具体 ExecuteTool 调用。这是为不存在的
// "LAM 向量→离散工具调用"路由特性预写的孤立算法实现（R1 禁止超前抽象/臆测开发）；
// 若未来真的要支持该路由，应随该特性的真实调用方一并设计。
