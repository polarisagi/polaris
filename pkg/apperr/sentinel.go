package apperr

// Sentinel errors — 全局哨兵错误，供 errors.Is 精确匹配。
//
// 哨兵错误分两类：
//   - 普通业务哨兵：可计入质量指标，允许重试
//   - 安全哨兵（ErrTaintViolation）：一票否决，必须触发 safety_fail，禁止归入不可控组
//   - 基础设施哨兵（ErrProviderExhausted / ErrNetworkUnavailable）：不可控失败，禁止计入质量指标
var (
	ErrNotImplemented    = New(CodeUnimplemented, "not implemented")
	ErrEmptyIndex        = New(CodeInternal, "index is empty (valid initial state)")
	ErrResourceExhausted = New(CodeResourceExhausted, "resource exhausted")
	ErrTimeout           = New(CodeTimeout, "operation timed out")
	ErrCancelled         = New(CodeCancelled, "operation cancelled")
	ErrUnauthorized      = New(CodeUnauthorized, "unauthorized")
	ErrForbidden         = New(CodeForbidden, "forbidden")
	ErrInvalidInput      = New(CodeInvalidInput, "invalid input")
	ErrNotFound          = New(CodeNotFound, "not found")
	ErrAlreadyExists     = New(CodeAlreadyExists, "already exists")
	ErrInternal          = New(CodeInternal, "internal error")

	// 基础设施不可控错误 — 禁止计入质量指标（非逻辑失败）。
	ErrProviderExhausted  = New(CodeProviderExhausted, "all LLM providers exhausted; non-logic failure")
	ErrNetworkUnavailable = New(CodeNetworkUnavailable, "network unavailable; non-logic failure")

	// 安全哨兵 — 一票否决：任何出现必须触发 safety_fail，禁止归入不可控组。
	// 污点违规是安全红线，必须计入安全指标。
	ErrTaintViolation = New(CodeTaintViolation, "taint gate rejected: external data entering instruction slot")

	// ErrTier0SandboxLimit Tier-0 硬件无法满足 L3 容器沙箱要求时返回。
	// M07 §4.2: 不提供静默降级，调用方必须显式处理或拒绝工具执行。
	ErrTier0SandboxLimit = New(CodeSandboxTier0Limit, "container sandbox requires Linux or Tier-1+ hardware")
)
