package guard

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
	"sync"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// tokenPattern 匹配 ⟦PII:xxxxxxxx⟧ 格式令牌，与 Tokenize 生成的格式严格一致。
var tokenPattern = regexp.MustCompile(`⟦PII:[0-9a-f]{8}⟧`)

// ErrUnknownPIIToken 在 Restore 遇到未知/伪造令牌时返回，调用方必须 fail-closed
// 拒绝执行，禁止把无法还原的令牌原样透传给下游工具（可能是攻击者伪造的探测载荷，
// 也可能是脱敏器状态已被清空导致的悬空引用，两种情况都不应该静默放行）。
var ErrUnknownPIIToken = apperr.New(apperr.CodeForbidden, "pii_token_vault: unknown or forged token, fail-closed")

// PIITokenVault 会话级轻量可逆令牌管理。
// 作用域限定在单次请求/单个 task 内，只存在内存里，不落盘。
type PIITokenVault struct {
	mu     sync.RWMutex
	tokens map[string]map[string]string // taskID -> token -> originalValue
}

func NewPIITokenVault() *PIITokenVault {
	return &PIITokenVault{
		tokens: make(map[string]map[string]string),
	}
}

// TokenizeForTask 记录原始值并返回一个人类可读的短 token，绑定到指定的 taskID。
func (v *PIITokenVault) TokenizeForTask(taskID string, original string) string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	shortID := hex.EncodeToString(b)

	token := fmt.Sprintf("⟦PII:%s⟧", shortID)

	v.mu.Lock()
	defer v.mu.Unlock()
	if v.tokens[taskID] == nil {
		v.tokens[taskID] = make(map[string]string)
	}
	v.tokens[taskID][token] = original

	return token
}

// Tokenize 记录原始值并返回一个人类可读的短 token。为了兼容性，绑定到全局共享 taskID。
// 建议使用 TokenizeForTask 代替。
func (v *PIITokenVault) Tokenize(original string) string {
	return v.TokenizeForTask("", original)
}

// ResolveForTask 根据 taskID 和 token 获取真实值，仅在 taskID 对应的命名空间内查找。
//
// 安全要求（2026-07-04 审计修复：原实现遇未知 token 会原样返回 token 本身，
// 是静默 fail-open——调用方若不检查返回值是否"看起来还是个token"，可能把
// 未解析的 token 字面量当作合法输入送进下游工具，或者反过来把伪造的
// ⟦PII:xxxx⟧ 输入误判为"没有对应真实值所以按原样处理"而放行）：
// 现在改为 fail-closed，未知 token 返回 ErrUnknownPIIToken，调用方必须拒绝
// 继续执行，不能静默降级。
//
// 不做跨 taskID 的回退查找（此前的实现在当前 taskID 桶未命中时会静默回退查
// 全局 "" 桶，这在生产环境 ctx 未正确携带 taskID 时会把"隔离失效"掩盖成
// "看起来正常工作"，与 fail-closed 的设计初衷矛盾）。若调用方传入的 taskID
// 与 TokenizeForTask 写入时不一致，本方法必须直接返回 ErrUnknownPIIToken，
// 而不是尝试用另一个命名空间的数据蒙混过关。
func (v *PIITokenVault) ResolveForTask(taskID string, token string) (string, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if taskTokens, ok := v.tokens[taskID]; ok {
		if val, exists := taskTokens[token]; exists {
			return val, nil
		}
	}
	return "", ErrUnknownPIIToken
}

// Resolve 根据 token 获取真实值。推荐使用 ResolveForTask。
func (v *PIITokenVault) Resolve(token string) (string, error) {
	return v.ResolveForTask("", token)
}

// Restore 扫描整段文本里的所有 ⟦PII:xxxx⟧ 令牌，逐一还原为真实值后返回。
// 任一令牌无法解析（未知/伪造）即整体 fail-closed 返回 error，不做部分还原
// ——部分还原会产生"一部分是假值占位符、一部分是真实值"的歧义文本，比整体
// 拒绝更危险。
//
// 安全要求（写入注释供后续维护者知晓，不可违反）：
//  1. 返回值只能存在于本次调用栈内，用于单次工具执行，不得写回任何
//     LLM 可见上下文；
//  2. 不得以明文形式记录到 EventLog / 审计日志；
//  3. 不得被 idempotencyCache 缓存或以任何形式持久化。
//
// RestoreForTask 扫描整段文本里的所有 ⟦PII:xxxx⟧ 令牌，仅从指定 taskID 的命名空间
// 逐一还原为真实值。不做跨 taskID 回退查找，理由同 ResolveForTask。
func (v *PIITokenVault) RestoreForTask(taskID string, text string) (string, error) {
	matches := tokenPattern.FindAllString(text, -1)
	if len(matches) == 0 {
		return text, nil
	}
	v.mu.RLock()
	defer v.mu.RUnlock()
	var outerErr error
	result := tokenPattern.ReplaceAllStringFunc(text, func(tok string) string {
		if outerErr != nil {
			return tok
		}
		var val string
		var ok bool
		if taskTokens, exists := v.tokens[taskID]; exists {
			val, ok = taskTokens[tok]
		}
		if !ok {
			outerErr = ErrUnknownPIIToken
			return tok
		}
		return val
	})
	if outerErr != nil {
		return "", outerErr
	}
	return result, nil
}

// Restore 扫描整段文本里的所有 ⟦PII:xxxx⟧ 令牌，逐一还原为真实值后返回。推荐使用 RestoreForTask。
func (v *PIITokenVault) Restore(text string) (string, error) {
	return v.RestoreForTask("", text)
}

// HasTokens 快速判断文本中是否包含 PII 令牌，供调用方在决定是否需要走
// Restore 之前做低成本预判（避免每次工具调用都无条件加锁扫描）。
func (v *PIITokenVault) HasTokens(text string) bool {
	return tokenPattern.MatchString(text)
}

// Clear 清空全局共享表的映射。
func (v *PIITokenVault) Clear() {
	v.ClearTask("")
}

// ClearTask 清空指定 taskID 的 PII 映射表，防止内存泄漏。
func (v *PIITokenVault) ClearTask(taskID string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	delete(v.tokens, taskID)
}
