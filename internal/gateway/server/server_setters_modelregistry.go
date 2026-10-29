package server

import (
	"github.com/polarisagi/polaris/internal/llm/modelregistry"
)

// SetModelRegistry 注入 P3-2 ModelVersionRegistry（M01 §9），供 providerHandler
// 的 HandleModelUpgrade/HandleModelDeprecate 运营触发入口使用（2026-07-21
// deadcode 审查补齐）。nil 时两个 Handle 方法直接返回 503，与其余可选依赖
// 一致的降级策略。
//
// 独立成文件的原因同 server_setters_sampling.go：避免继续膨胀 server_core.go。
func (s *Server) SetModelRegistry(r *modelregistry.Registry) {
	if s.providerHandler != nil {
		s.providerHandler.ModelRegistry = r
	}
}
