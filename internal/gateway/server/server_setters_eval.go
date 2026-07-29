package server

import (
	"github.com/polarisagi/polaris/internal/gateway/server/sysadmin/evaladmin"
	"github.com/polarisagi/polaris/internal/protocol"
)

// SetEvalAdmin 注入 V8-S2 Meta-Eval Sentinel 运维接口（NewServer 之后、Start 之前
// 调用）。store/sentinel 来自 AgentBundle（boot_agent.go 构造，晚于 NewServer）。
// 对已存在的 *evaladmin.EvalAdmin 做原地字段回填，而非替换整个指针——
// SetEvalAdmin 挂载 Meta-Eval Sentinel 独立运维接口（V8-S2 架构）。
func (s *Server) SetEvalAdmin(store evaladmin.EvalStore, sentinel evaladmin.MetaAuditor, runner protocol.EvalRunner) {
	s.sysadminHandler.Eval.Store = store
	s.sysadminHandler.Eval.Sentinel = sentinel
	s.sysadminHandler.Eval.Runner = runner
}
