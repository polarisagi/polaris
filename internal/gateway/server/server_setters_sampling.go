package server

import (
	"github.com/polarisagi/polaris/internal/eval/analysis"
)

// SetSamplingMonitor 注入连续采样退化监控（M12 §9），与 EvalRunner 共享同一
// 进程级单例（cmd/polaris/boot_agent.go 构造）。nil 时 ChatHandler.
// SampleAndScoreReply 直接跳过（不采样、不计入退化窗口），与其它可选依赖
// 一致的降级策略。
//
// 独立成文件的原因：server_core.go 已逼近 R7 400 行上限，本 setter 与其余
// 后置注入方法并无耦合，单独拆出即可释放余量（与 server_setters_eval.go
// 同一拆分先例）。
func (s *Server) SetSamplingMonitor(m *analysis.ContinuousSamplingMonitor) {
	if s.chatHandler != nil {
		s.chatHandler.SamplingMonitor = m
	}
}
