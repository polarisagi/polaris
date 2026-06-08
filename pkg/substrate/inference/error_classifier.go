package inference

import (
	"regexp"
	"strconv"
	"strings"
)

// FailReason 精确错误原因枚举，比 substrate.FailMode 粒度更细。
// 每个值对应唯一的恢复策略，上层无需重复解析错误文本。
type FailReason string

const (
	// ── 认证类 ──────────────────────────────────────────────────────────────
	ReasonAuth          FailReason = "auth"           // 401/403 瞬时 → 轮换凭证
	ReasonAuthPermanent FailReason = "auth_permanent" // 认证刷新失败 → 终止，不可恢复

	// ── 计费类 ──────────────────────────────────────────────────────────────
	ReasonBilling FailReason = "billing" // 402 / 余额耗尽 → 立即轮换凭证

	// ── 限流类 ──────────────────────────────────────────────────────────────
	ReasonRateLimit  FailReason = "rate_limit" // 429 → 退避后轮换凭证
	ReasonOverloaded FailReason = "overloaded" // 503/529 → 退避但不换 provider

	// ── 服务器类 ────────────────────────────────────────────────────────────
	ReasonServerError FailReason = "server_error" // 500/502 → 重试同 provider
	ReasonTimeout     FailReason = "timeout"      // 网络超时 → 重试同 provider

	// ── 内容/负载类 ──────────────────────────────────────────────────────────
	ReasonContextOverflow FailReason = "context_overflow"  // 上下文超限 → 压缩后重试
	ReasonPayloadTooLarge FailReason = "payload_too_large" // 413 → 裁剪 payload 后重试
	ReasonImageTooLarge   FailReason = "image_too_large"   // 图像过大 → 缩小后重试

	// ── 模型/格式类 ──────────────────────────────────────────────────────────
	ReasonModelNotFound         FailReason = "model_not_found"         // 404 → 换模型，非换 provider
	ReasonFormatError           FailReason = "format_error"            // 400 格式错误 → 终止
	ReasonProviderPolicyBlocked FailReason = "provider_policy_blocked" // 策略拦截 → 终止，不换凭证

	// ── Anthropic 特有 ───────────────────────────────────────────────────────
	ReasonThinkingSignature         FailReason = "thinking_signature"           // 思考块签名验证失败 → 重试
	ReasonLongContextTier           FailReason = "long_context_tier"            // 需解锁长上下文 tier → 换模型
	ReasonOAuthLongContextForbidden FailReason = "oauth_long_context_forbidden" // OAuth beta 禁用 → 禁用 beta

	// ── Ollama/llama.cpp 特有 ────────────────────────────────────────────────
	ReasonLlamaCppGrammar FailReason = "llama_cpp_grammar" // grammar 与 regex tool 冲突 → 移除 regex

	// ── 兜底 ─────────────────────────────────────────────────────────────────
	ReasonUnknown FailReason = "unknown" // 带退避重试
)

// ClassifiedError 携带精确错误原因和恢复提示。
// 调用方按布尔字段决策，无需再解析 Reason 字符串。
type ClassifiedError struct {
	Reason   FailReason
	Status   int               // HTTP 状态码；0 表示非 HTTP 错误（网络层、超时）
	Provider string            // 来源 provider（可选，由调用方注入）
	Message  string            // 截断后的原始错误消息（用于日志，已脱敏截断）
	Context  map[string]string // 额外诊断字段（供调试使用）

	// 恢复提示：调用方直接读字段，不 switch Reason。
	Retryable              bool // 值得重试（包含 backoff 场景）
	ShouldCompress         bool // 先压缩 context 再重试（context_overflow/payload）
	ShouldRotateCredential bool // 轮换到下一个凭证再重试
	ShouldFallback         bool // 换模型（非换 provider）
}

// IsAuth 报告错误是否属于认证类（瞬时或永久）。
func (e ClassifiedError) IsAuth() bool {
	return e.Reason == ReasonAuth || e.Reason == ReasonAuthPermanent
}

// FallbackTierInt 将 Reason 映射到 substrate.FallbackTier 的整型值。
// 返回 int 以避免 inference → substrate 循环引用。
// 对应值：0=Primary 1=Secondary 2=Tertiary 3=Graceful 4=Escalate。
func (e ClassifiedError) FallbackTierInt() int {
	switch e.Reason {
	case ReasonRateLimit, ReasonAuth, ReasonBilling, ReasonServerError:
		return 1 // Secondary：换备选凭证/provider 立即重试
	case ReasonTimeout, ReasonContextOverflow, ReasonPayloadTooLarge,
		ReasonOverloaded, ReasonLlamaCppGrammar, ReasonImageTooLarge:
		return 2 // Tertiary：本地处理（压缩/缩图/退避）后重试同 provider
	case ReasonAuthPermanent, ReasonFormatError, ReasonProviderPolicyBlocked:
		return 4 // Escalate：不可恢复，向上报告
	default: // ModelNotFound, ThinkingSignature, LongContextTier, OAuthForbidden, Unknown
		return 3 // Graceful：换模型或带退避重试
	}
}

// Classify 从 error 提取 FailReason 和恢复提示。
//
// 支持的 polaris adapter 错误格式：
//   - Anthropic:       "[INTERNAL] anthropic: HTTP {code}: {body}"
//   - OpenAI 兼容:    "[INTERNAL] api error (status {code}): {body}"
//   - 网络/超时:      标准 Go net 错误消息
func Classify(err error) ClassifiedError {
	if err == nil {
		return ClassifiedError{Reason: ReasonUnknown, Message: "<nil>"}
	}
	msg := err.Error()
	lower := strings.ToLower(msg)
	status := extractHTTPStatus(msg)

	// 截断过长消息，防止日志膨胀
	display := msg
	if len(display) > 400 {
		display = display[:400] + "…"
	}
	base := ClassifiedError{Status: status, Message: display}
	return doClassify(base, status, lower)
}

// ClassifyWithProvider 同 Classify，附加 provider 来源标签。
func ClassifyWithProvider(err error, provider string) ClassifiedError {
	ce := Classify(err)
	ce.Provider = provider
	return ce
}

// ─── 内部实现 ──────────────────────────────────────────────────────────────────

// reHTTPStatus 匹配三种 adapter 错误格式中的 HTTP 状态码：
//
//	"anthropic: HTTP 429:"      → 匹配 "HTTP 429"
//	"api error (status 429):"   → 匹配 "status 429"（\b 兼容 "(" 前缀）
//	"status=401 unauthorized"   → 匹配 "status=401"（\b 兼容行首）
var reHTTPStatus = regexp.MustCompile(`(?:HTTP\s+|\bstatus[=\s])(\d{3})`)

func extractHTTPStatus(msg string) int {
	m := reHTTPStatus.FindStringSubmatch(msg)
	if len(m) == 2 {
		n, _ := strconv.Atoi(m[1])
		return n
	}
	return 0
}

func doClassify(base ClassifiedError, status int, lower string) ClassifiedError {
	// ── 无 HTTP 状态码：网络层错误 ────────────────────────────────────────────
	if status == 0 {
		if containsAny(lower,
			"deadline exceeded", "context deadline",
			"timeout", "timed out", "i/o timeout",
			"operation timed out",
		) {
			return hints(base, ReasonTimeout, true, false, false, false)
		}
		if containsAny(lower,
			"connection refused", "no such host",
			"network unreachable", "connection reset",
			"dial tcp", "read tcp",
		) {
			return hints(base, ReasonServerError, true, false, false, false)
		}
		return hints(base, ReasonUnknown, true, false, false, false)
	}

	// ── 按 HTTP 状态码分发 ────────────────────────────────────────────────────
	switch {
	case status == 401 || status == 403:
		return classifyAuth(base, lower)
	case status == 402:
		return hints(base, ReasonBilling, false, false, true, false)
	case status == 404:
		return classifyNotFound(base, lower)
	case status == 413:
		return classifyPayloadTooLarge(base, lower)
	case status == 429:
		return classifyRateLimitOrBilling(base, lower)
	case status == 400:
		return classifyBadRequest(base, lower)
	case status == 503 || status == 529:
		// 529 是 Anthropic 的"过载"状态码
		return hints(base, ReasonOverloaded, true, false, false, false)
	case status >= 500:
		return hints(base, ReasonServerError, true, false, false, false)
	default:
		return hints(base, ReasonUnknown, true, false, false, false)
	}
}

// classifyAuth 区分瞬时认证失败和永久认证失败，以及 403 策略拦截。
func classifyAuth(base ClassifiedError, lower string) ClassifiedError {
	// 永久认证：token 撤销、刷新失败
	if containsAny(lower,
		"refresh token expired", "permanently invalid",
		"cannot refresh", "token revoked", "credentials revoked",
	) {
		return hints(base, ReasonAuthPermanent, false, false, false, false)
	}
	// 403 + 策略关键词 → provider 策略拦截，不是认证问题
	if base.Status == 403 && containsAny(lower,
		"data polic", "terms of service", "content polic",
		"acceptable use", "usage polic", "safety polic",
	) {
		return hints(base, ReasonProviderPolicyBlocked, false, false, false, false)
	}
	// 瞬时认证失败 → 轮换凭证重试
	return hints(base, ReasonAuth, false, false, true, false)
}

// classifyNotFound 区分模型不存在和其他 404。
func classifyNotFound(base ClassifiedError, lower string) ClassifiedError {
	if containsAny(lower, "model", "engine", "deployment", "model_not_found") {
		return hints(base, ReasonModelNotFound, false, false, false, true)
	}
	return hints(base, ReasonFormatError, false, false, false, false)
}

// classifyPayloadTooLarge 区分图像过大和普通负载超限。
func classifyPayloadTooLarge(base ClassifiedError, lower string) ClassifiedError {
	if containsAny(lower, "image", "vision", "multimodal", "base64") {
		return hints(base, ReasonImageTooLarge, true, false, false, false)
	}
	return hints(base, ReasonPayloadTooLarge, true, true, false, false)
}

// classifyRateLimitOrBilling 区分 429 的两种语义。
//
// "usage limit" 后跟 "try again"/"reset in" → rate_limit（临时）；
// "usage limit" 无重试提示 → billing（配额耗尽，需换凭证）。
func classifyRateLimitOrBilling(base ClassifiedError, lower string) ClassifiedError {
	// 明确的限流关键词
	if containsAny(lower,
		"rate limit", "rate_limit", "ratelimit",
		"throttl",
		"requests per minute", "tokens per minute", "rpm", "tpm",
		"resource_exhausted", // Google gRPC 映射
		"too many requests",
		"request limit exceeded", // 阿里云 / 火山引擎
		"流量超限", "超过请求速率",
	) {
		return hints(base, ReasonRateLimit, true, false, true, false)
	}
	// "usage limit" 歧义：有重试提示 → 限流；否则 → 计费
	if strings.Contains(lower, "usage limit") {
		if containsAny(lower, "try again", "reset in", "retry after") {
			return hints(base, ReasonRateLimit, true, false, true, false)
		}
		return hints(base, ReasonBilling, false, false, true, false)
	}
	// 兜底 429 视为限流
	return hints(base, ReasonRateLimit, true, false, true, false)
}

// classifyBadRequest 细分 HTTP 400：上下文超限 / Anthropic 特有 / llama.cpp grammar / 格式错误。
func classifyBadRequest(base ClassifiedError, lower string) ClassifiedError {
	// ── 上下文超限（跨 provider 的多种表述）───────────────────────────────────
	if containsAny(lower,
		"context length", "context_length_exceeded",
		"maximum context length", "maximum sequence length", // vLLM
		"input is too long", "input too long",
		"prompt is too long", // Ollama
		"too many tokens",    // llama.cpp
		"input exceeds maximum", "exceeds the maximum",
		"tokens in the input", // Bedrock
		"reduce the length", "shorten your",
		"input token count",     // Google: "Input token count (X) exceeds this model's limit"
		"context window",        // Google: "exceeds this model's context window"
		"超出最大 token", "超过上下文长度", // 中文 provider
	) {
		return hints(base, ReasonContextOverflow, true, true, false, false)
	}

	// ── Anthropic：思考块签名验证失败 ─────────────────────────────────────────
	if containsAny(lower,
		"thinking block", "thinking signature", "extended thinking signature",
	) {
		return hints(base, ReasonThinkingSignature, true, false, false, false)
	}

	// ── Anthropic：需解锁长上下文 tier ───────────────────────────────────────
	if containsAny(lower, "long context tier", "long-context tier", "32k context") {
		return hints(base, ReasonLongContextTier, false, false, false, true)
	}

	// ── Anthropic：OAuth 长上下文 beta 禁用 ──────────────────────────────────
	if containsAny(lower, "long-context beta", "long context beta", "oauth_long_context") {
		return hints(base, ReasonOAuthLongContextForbidden, false, false, false, false)
	}

	// ── Ollama/llama.cpp：grammar 与 regex tool 冲突 ──────────────────────────
	// 需同时包含 grammar 关键词和冲突指示词，避免误判
	if containsAny(lower, "grammar", "gguf grammar") &&
		containsAny(lower, "regex", "pattern", "conflict", "unsupported") {
		return hints(base, ReasonLlamaCppGrammar, true, false, false, false)
	}

	// ── 通用 400 格式错误 ─────────────────────────────────────────────────────
	return hints(base, ReasonFormatError, false, false, false, false)
}

// hints 填充恢复提示字段并返回完整 ClassifiedError。
func hints(base ClassifiedError, reason FailReason, retryable, compress, rotate, fallback bool) ClassifiedError {
	base.Reason = reason
	base.Retryable = retryable
	base.ShouldCompress = compress
	base.ShouldRotateCredential = rotate
	base.ShouldFallback = fallback
	return base
}

// containsAny 报告 s 是否包含 needles 中的任意子串。
// s 应已转为小写；needles 本身也应为小写。
func containsAny(s string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}
