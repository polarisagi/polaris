package security

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

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

var _ protocol.SecurityFacade = (*SecurityFacadeImpl)(nil)

// NewSecurityFacade 构造安全门面。
// ks/audit 任一为 nil 时对应功能 fail-safe 降级（参见各方法注释）。
// policy 必须非 nil，为 nil 时 IsAuthorized 恒定拒绝（fail-closed），调用方需自行决定是否允许系统在无策略引擎时启动。
func NewSecurityFacade(ks *KillSwitch, audit *AuditTrail, policy PolicyEvaluator) *SecurityFacadeImpl {
	return &SecurityFacadeImpl{ks: ks, audit: audit, policy: policy}
}

func (f *SecurityFacadeImpl) KillState() types.KillState {
	if f.ks == nil {
		return types.KillNormal
	}
	return f.ks.GetState()
}

func (f *SecurityFacadeImpl) IsOperational() bool {
	return f.KillState() <= types.KillThrottle
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
		slog.Warn("security: audit trail not configured, record dropped", "action", action)
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
		return false, apperr.New(apperr.CodeInternal, "security: policy engine not configured (fail-closed)")
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
