package protocol

import (
	"context"

	"github.com/polarisagi/polaris/pkg/types"
)

// SecurityFacade 安全模块统一对外接口。
//
// 设计原则：
//   - 其他模块只依赖此接口，不直接操作 KillSwitch/AuditTrail/PolicyGate 具体 struct
//   - 接口方法覆盖三个子系统：KillSwitch 感知、审计写入、策略决策
//   - 污点操作通过 taint 包的类型系统在编译期强制，不放入此接口
//
// @consumer: agent/agent.go, gateway/server/server.go, swarm/orchestrator/orchestrator.go
// @producer: SecurityFacadeImpl（由 cli.go/bootstrap 构造并注入 DependencyMap）
type SecurityFacade interface {
	// KillState 返回当前紧急停止状态（Normal/Throttle/Pause/FullStop）。
	KillState() types.KillState
	// IsOperational 返回系统是否处于可执行状态（<=KillThrottle）。
	IsOperational() bool
	// ReportError 上报单次错误，自动触发阈值检查（连续 >5 → Throttle）。
	ReportError()
	// ReportSafetyViolation 上报安全违规（fatal=true → 直接 FullStop）。
	ReportSafetyViolation(fatal bool)
	// ManualFullStop 管理员手动触发全停。
	ManualFullStop(actor, reason string)

	// LogAudit 写入操作审计日志。失败不影响主流程（仅记录错误日志）。
	LogAudit(ctx context.Context, action string, meta map[string]any) error

	// IsAuthorized 向 Cedar 策略引擎查询是否授权（deny-by-default）。
	// FFI 调用爆发或超时 → deny（fail-closed）。
	IsAuthorized(ctx context.Context, principal, action, resource string, ctxData map[string]any) (bool, error)

	// MaxTaint 返回多个污点等级中的最高级（请求级污点聚合）。
	MaxTaint(levels ...types.TaintLevel) types.TaintLevel
}
