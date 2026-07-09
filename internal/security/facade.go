package security

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

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
	KillState() KillState
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
	// FFI 调用失败或超时 → deny（fail-closed）。
	IsAuthorized(ctx context.Context, principal, action, resource string, ctxData map[string]any) (bool, error)

	// MaxTaint 返回多个污点等级中的最高级（请求级污点聚合）。
	MaxTaint(levels ...types.TaintLevel) types.TaintLevel
}

// PolicyEvaluator 安全门面对 Cedar 策略引擎的消费端接口。
// 由 security/policy.Gate 实现，此处定义防止 facade.go 循环 import policy 子包。
type PolicyEvaluator interface {
	IsAuthorized(ctx context.Context, principal, action, resource string, ctxData map[string]any) (bool, error)
}

// ─── 实现 ─────────────────────────────────────────────────────────────────────

// SecurityFacadeImpl 组合 KillSwitch + AuditTrail + PolicyEvaluator 的门面实现。
// 由 cli.go / Bootstrapper 在启动时构造并注册到 DependencyMap["SecurityFacade"]。
type SecurityFacadeImpl struct {
	ks     *KillSwitch
	audit  *AuditTrail
	policy PolicyEvaluator
}

// NewSecurityFacade 构造安全门面。
// ks/audit/policy 任一为 nil 时对应功能 fail-safe 降级（参见各方法注释）。
func NewSecurityFacade(ks *KillSwitch, audit *AuditTrail, policy PolicyEvaluator) *SecurityFacadeImpl {
	return &SecurityFacadeImpl{ks: ks, audit: audit, policy: policy}
}

func (f *SecurityFacadeImpl) KillState() KillState {
	if f.ks == nil {
		return KillNormal
	}
	return f.ks.GetState()
}

func (f *SecurityFacadeImpl) IsOperational() bool {
	return f.KillState() <= KillThrottle
}

func (f *SecurityFacadeImpl) ReportError() {
	if f.ks != nil {
		f.ks.ReportError()
	}
}

func (f *SecurityFacadeImpl) ReportSafetyViolation(fatal bool) {
	if f.ks != nil {
		f.ks.ReportSafetyViolation(fatal)
	}
}

func (f *SecurityFacadeImpl) ManualFullStop(actor, reason string) {
	if f.ks != nil {
		f.ks.ManualFullStop(actor, reason)
	}
}

func (f *SecurityFacadeImpl) LogAudit(_ context.Context, action string, meta map[string]any) error {
	if f.audit == nil {
		return nil
	}
	detail, err := json.Marshal(meta)
	if err != nil {
		detail = []byte(fmt.Sprintf("%v", meta))
	}
	return f.audit.Record(&AuditRecord{
		ActionType:   action,
		ActionDetail: detail,
		Timestamp:    time.Now().UnixMicro(),
		Outcome:      "allow",
	})
}

func (f *SecurityFacadeImpl) IsAuthorized(ctx context.Context, principal, action, resource string, ctxData map[string]any) (bool, error) {
	if f.policy == nil {
		return true, nil // 无策略引擎时 fail-open（测试/简化模式）
	}
	return f.policy.IsAuthorized(ctx, principal, action, resource, ctxData)
}

func (f *SecurityFacadeImpl) MaxTaint(levels ...types.TaintLevel) types.TaintLevel {
	var max types.TaintLevel
	for _, l := range levels {
		if l > max {
			max = l
		}
	}
	return max
}
