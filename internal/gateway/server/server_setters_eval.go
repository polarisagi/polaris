package server

import (
	"github.com/polarisagi/polaris/internal/gateway/server/sysadmin/evaladmin"
)

// SetEvalAdmin 注入 V8-S2 Meta-Eval Sentinel 运维接口（NewServer 之后、Start 之前
// 调用）。store/sentinel 来自 AgentBundle（boot_agent.go 构造，晚于 NewServer）。
// 对已存在的 *evaladmin.EvalAdmin 做原地字段回填，而非替换整个指针——
// server_routes.go 注册路由时捕获的是 s.sysadminHandler.Eval 这个指针本身，
// 必须保持稳定，否则回填对已注册路由不可见（与 SetInstallManager 回填
// mcpadmin.InstallMgr 字段是同一模式）。任一为 nil 时 evaladmin 对应 handler
// 返回 503，不 panic。
//
// 独立成文件的原因：server_core.go 已逼近 R7 400 行上限，本 setter 与其余
// 后置注入方法并无耦合，单独拆出即可释放余量，无需拆分其余 setter。
func (s *Server) SetEvalAdmin(store evaladmin.EvalStore, sentinel evaladmin.MetaAuditor) {
	s.sysadminHandler.Eval.Store = store
	s.sysadminHandler.Eval.Sentinel = sentinel
}
