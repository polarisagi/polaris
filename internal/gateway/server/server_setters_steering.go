package server

import (
	llmadapter "github.com/polarisagi/polaris/internal/llm/adapter"
)

// SetSteering 注入激活引导适配器与控制向量注册表（M09 §1.3，2026-07-21
// deadcode 审查补齐 /steer 命令面）。两者均可为 nil（FeatureActivationSteer
// 未启用/Tier<1 时），SlashCommandRouter.handleSteer 对 nil 依赖 nil-safe
// 降级提示，不影响其余斜线命令，与 SetSamplingMonitor 同一注入风格。
func (s *Server) SetSteering(steering *llmadapter.SteeringAdapter, cvStore *llmadapter.ControlVectorStore) {
	if s.chatHandler != nil && s.chatHandler.SlashRouter != nil {
		s.chatHandler.SlashRouter.SetSteering(steering, cvStore)
	}
}
