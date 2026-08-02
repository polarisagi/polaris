package policy

// 2026-08-02 从 gate.go 拆分（Test_inv_FileLineLimit R7 400 行上限存量债务，见
// local_playground/upgrade/99-new-findings.md 阶段03 R-07 发现），纯搬运无行为变更。
// 本文件收敛出口污点检查（M11 §2.3/§6 TaintMedium 硬地板 + HITL 放行豁免）。

import (
	"github.com/polarisagi/polaris/internal/security/token"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// TaintEgressCheck 检查 Taint 出口：TaintMedium 级别数据不可直接输出到外部接口。
// 违反 → ErrTaintBlockedEgress（对应 M11 §2.3 SanitizeBySchema 规则）。
func (g *Gate) TaintEgressCheck(levels ...types.TaintLevel) error {
	if g == nil {
		return apperr.New(apperr.CodeInternal, "policy: nil receiver")
	}
	result := types.PropagateTaint(levels...)
	// TaintMedium 硬地板：Medium 及以上级别数据不得直接出口，必须经过清洗
	if result >= types.TaintMedium {
		return ErrTaintBlockedEgress
	}
	return nil
}

// CheckEgressWithExemption 出口污点检查，支持 HITL 放行令牌。
// token 为 nil 时退化为 CheckEgress（无放行通道）。
// 拦截时返回的错误是 *TaintEgressBlockedError（携带被拦截的原始 data，供上游
// M04 §3 转义路径铸造 TaintExemptionToken 时使用——豁免令牌的哈希必须精确匹配
// 被拦截的字节内容，不能用人类可读摘要代替），其 Unwrap() 仍指向
// ErrTaintBlockedEgress，errors.Is(err, ErrTaintBlockedEgress) 不受影响。
func (g *Gate) CheckEgressWithExemption(data []byte, taintLevel types.TaintLevel, tok *token.TaintExemptionToken) error {
	if g == nil {
		return apperr.New(apperr.CodeInternal, "policy: nil receiver")
	}
	if taintLevel < types.TaintMedium {
		return nil
	}
	// HITL 放行：令牌有效则放行，并记录审计事件
	if tok != nil && tok.Valid(data) {
		return nil // 已放行，审计由调用方通过 EventLog 记录 token.Summary()
	}
	return &TaintEgressBlockedError{Data: data}
}

// TaintEgressBlockedError 包裹 ErrTaintBlockedEgress 并携带被拦截的原始数据。
type TaintEgressBlockedError struct {
	Data []byte
}

func (e *TaintEgressBlockedError) Error() string { return ErrTaintBlockedEgress.Error() }
func (e *TaintEgressBlockedError) Unwrap() error { return ErrTaintBlockedEgress }
