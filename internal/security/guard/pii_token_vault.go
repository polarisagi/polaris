package guard

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"
)

const (
	// piiVaultIdleTTL 命名空间闲置多久后被 ReleaseTask 的清扫回收。令牌映射按"会话"而非"回合"
	// 存活，才能让跨回合的历史/画像/核心记忆字节一致（见 ReleaseTask）；闲置回收防止会话结束后
	// 仍无界驻留。原文本就驻留在会话历史与本映射里，保留期内未新增暴露面。
	piiVaultIdleTTL = 6 * time.Hour
	// piiVaultMaxTasks 同时保留的命名空间数上限（有界），超出按最久未用淘汰。
	piiVaultMaxTasks = 1024
)

// tokenPattern 匹配 ⟦PII:xxxxxxxx⟧ 格式令牌，与 Tokenize 生成的格式严格一致。
var tokenPattern = regexp.MustCompile(`⟦PII:[0-9a-f]{8}⟧`)

// ErrUnknownPIIToken 在 Restore 遇到未知/伪造令牌时返回，调用方必须 fail-closed
// 拒绝执行，禁止把无法还原的令牌原样透传给下游工具（可能是攻击者伪造的探测载荷，
// 也可能是脱敏器状态已被清空导致的悬空引用，两种情况都不应该静默放行）。
var ErrUnknownPIIToken = apperr.New(apperr.CodeForbidden, "pii_token_vault: unknown or forged token, fail-closed")

// PIITokenVault 会话级轻量可逆令牌管理。
// 作用域限定在单次请求/单个 task 内，只存在内存里，不落盘。
//
// 确定性会话内令牌（ADR-0105 决策十一）：同一 taskID 内同一原文始终得到同一令牌，
// 使 LLM 请求里含 PII 的历史/画像/核心记忆在每次调用间字节一致（Provider 前缀缓存
// 不在此处断开），模型也能识别"同一个邮箱/同一个人"。跨 taskID 令牌仍由 crypto/rand
// 独立生成，互不可关联。
//
// 安全审查结论（WP9）：
//  1. 令牌相等性泄露的信息（"这两处是同一个值"）在同一会话内本就可从上下文推断，
//     且令牌与原文之间无任何可计算关系（纯随机、非哈希/HMAC，无法做字典攻击）；
//  2. 跨会话不可关联：令牌按 taskID 各自独立随机，同一原文在两个会话里得到不同令牌；
//  3. ResolveForTask 只查本 taskID 的正向映射，fail-closed 语义不变；
//  4. 反向映射 original→token 与正向映射同锁、同生命周期（ClearTask 一并删除），
//     不额外持久化；原文本来就驻留在正向映射里，未新增暴露面；
//  5. 碰撞：此前令牌重复会静默覆盖另一原文的映射（还原出错误的 PII），
//     现在碰撞时重试生成（令牌只有 32 bit 熵，n 个不同值的同会话碰撞概率约 n²/2³³，n=1 万时约 1.2%）。
//
// 有意不做规范化：按检测器输出的原文精确匹配。检测器为正则规则，对同一实体的
// 大小写/空白/分隔符不同写法（Alice@X.com vs alice@x.com、138 0000 1111 vs
// 13800001111）给出不同原文，这些得到不同令牌——代价只是缓存命中略低，而规范化
// 有把不同实体合并的风险（邮箱本地部分在部分系统大小写敏感），合并是不可逆的
// 信息损失，宁可保守。
type PIITokenVault struct {
	mu      sync.RWMutex
	tokens  map[string]map[string]string // taskID -> token -> originalValue
	reverse map[string]map[string]string // taskID -> originalValue -> token（与 tokens 同锁同生命周期）
	lastUse map[string]time.Time         // taskID -> 创建/最近一次 ReleaseTask 时间（闲置清扫依据）
	// randHex 令牌随机源，测试可替换以制造碰撞；生产恒为 secureRandomHex。
	randHex func(n int) string
	// now 时钟，测试可替换；生产恒为 time.Now。
	now func() time.Time
}

func NewPIITokenVault() *PIITokenVault {
	return &PIITokenVault{
		tokens:  make(map[string]map[string]string),
		reverse: make(map[string]map[string]string),
		lastUse: make(map[string]time.Time),
		randHex: secureRandomHex,
		now:     time.Now,
	}
}

// TokenizeForTask 记录原始值并返回一个人类可读的短 token，绑定到指定的 taskID。
// 同一 taskID 内同一原文重复调用返回同一令牌（确定性会话内令牌）。
func (v *PIITokenVault) TokenizeForTask(taskID string, original string) string {
	// 快路径：读锁命中反向映射（每次 LLM 调用对全部历史消息都会走到这里）。
	v.mu.RLock()
	if tok, ok := v.reverse[taskID][original]; ok {
		v.mu.RUnlock()
		return tok
	}
	v.mu.RUnlock()

	v.mu.Lock()
	defer v.mu.Unlock()
	// 双检：读锁释放到写锁获取之间可能已被并发写入。
	if tok, ok := v.reverse[taskID][original]; ok {
		return tok
	}
	if v.tokens[taskID] == nil {
		v.tokens[taskID] = make(map[string]string)
		v.reverse[taskID] = make(map[string]string)
		v.lastUse[taskID] = v.now()
	}
	// 4 字节 → 8 位小写 hex，与 tokenPattern ⟦PII:[0-9a-f]{8}⟧ 严格对应。
	// 熵源不可用时 secureRandomHex fail-fast，绝不用可预测值生成令牌——
	// 全零 shortID 会让同一 task 内所有 PII 映射塌缩成一个 key 互相覆盖。
	// 碰撞检测：已占用的令牌重新生成，避免覆盖另一原文的映射。
	var token string
	// 无上限重试：每次成功概率 ≥ 1 - n/2³²，期望远小于 2 次；熵源损坏时
	// secureRandomBytes 自身会 panic，不会在此空转。
	for {
		token = fmt.Sprintf("⟦PII:%s⟧", v.randHex(4))
		if _, taken := v.tokens[taskID][token]; !taken {
			break
		}
	}
	v.tokens[taskID][token] = original
	v.reverse[taskID][original] = token
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

// RestoreForTask 扫描整段文本里的所有 ⟦PII:xxxx⟧ 令牌，仅从指定 taskID 的命名空间
// 逐一还原为真实值。不做跨 taskID 回退查找，理由同 ResolveForTask。
//
// 任一令牌无法解析（未知/伪造）即整体 fail-closed 返回 error，不做部分还原
// ——部分还原会产生"一部分是假值占位符、一部分是真实值"的歧义文本，比整体
// 拒绝更危险。
//
// 安全要求（写入注释供后续维护者知晓，不可违反）：
//  1. 返回值只能存在于本次调用栈内，用于单次工具执行，不得写回任何
//     LLM 可见上下文；
//  2. 不得以明文形式记录到 EventLog / 审计日志；
//  3. 不得被 idempotencyCache 缓存或以任何形式持久化。
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

// TokenizeKnownValues 是 RestoreForTask 的反方向操作：扫描 text，把其中出现的、
// 已在指定 taskID 命名空间登记过的"真实值"重新替换回对应的 ⟦PII:xxxx⟧ 令牌。
//
// 使用场景（2026-07-11 复核修复 GR-6-005）：工具执行前 execInput 已被 RestoreForTask
// 还原为真实值传给沙箱/下游进程，如果该工具的 Error/Output 把入参原样回显（例如
// CLI 参数校验失败时把命令行打印进 stderr），真实 PII 会经由 ExecuteTool 的返回值
// 泄漏给上游（LLM 上下文/审计日志）。此前的实现误用 RestoreForTask（token→真实值）
// 处理输出，对已经是真实值的文本是纯粹的 no-op，起不到任何脱敏作用。
// TokenizeKnownValues 才是这里需要的方向：真实值→token。
//
// 只替换本次任务命名空间内已知的原始值，不做全局扫描（避免误伤其他任务/其他
// 请求中恰好相同的普通文本，以及避免跨任务的信息串扰）。按原始值长度降序替换，
// 防止短值是长值子串时的部分替换错位。
func (v *PIITokenVault) TokenizeKnownValues(taskID string, text string) string {
	if text == "" {
		return text
	}
	v.mu.RLock()
	taskReverse := v.reverse[taskID]
	if len(taskReverse) == 0 {
		v.mu.RUnlock()
		return text
	}
	// 复制一份 value→token 映射，尽快释放锁，不在持锁期间做字符串替换。
	reverse := make(map[string]string, len(taskReverse))
	originals := make([]string, 0, len(taskReverse))
	for original, tok := range taskReverse {
		if original == "" {
			continue
		}
		reverse[original] = tok
		originals = append(originals, original)
	}
	v.mu.RUnlock()

	if len(originals) == 0 {
		return text
	}
	sort.Slice(originals, func(i, j int) bool { return len(originals[i]) > len(originals[j]) })

	result := text
	for _, original := range originals {
		if strings.Contains(result, original) {
			result = strings.ReplaceAll(result, original, reverse[original])
		}
	}
	return result
}

// Clear 清空全局共享表的映射。
func (v *PIITokenVault) Clear() {
	v.ClearTask("")
}

// ClearTask 立即清空指定 taskID 的 PII 映射表（显式销毁）。
func (v *PIITokenVault) ClearTask(taskID string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.dropLocked(taskID)
}

func (v *PIITokenVault) dropLocked(taskID string) {
	delete(v.tokens, taskID)
	delete(v.reverse, taskID)
	delete(v.lastUse, taskID)
}

// ReleaseTask 回合终态调用：**保留**该 taskID 的映射供同一会话的后续回合复用，只登记"最近使用"
// 并顺带清扫闲置超过 piiVaultIdleTTL 的命名空间、把总数压回 piiVaultMaxTasks（按最久未用淘汰）。
//
// 为什么不在回合终态 ClearTask：Agent 每回合一个新实例，终态即清空令牌映射，下一回合同一
// PII 原文得到全新随机令牌——会话历史、用户画像、核心记忆里凡含 PII 的消息字节全部改变，
// L0..L2 前缀缓存在首个 PII 处断开（2026-09-30 真实请求边界门控实测：3 个 PII 原文在 3 回合里
// 共出现 9 个令牌）。令牌映射因此按会话存活；有界性由闲置 TTL 与数量上限保证。
func (v *PIITokenVault) ReleaseTask(taskID string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	now := v.now()
	if _, ok := v.tokens[taskID]; ok {
		v.lastUse[taskID] = now
	}
	for id, at := range v.lastUse {
		if id != taskID && now.Sub(at) > piiVaultIdleTTL {
			v.dropLocked(id)
		}
	}
	for len(v.lastUse) > piiVaultMaxTasks {
		oldest, oldestAt := "", now
		for id, at := range v.lastUse {
			if id != taskID && !at.After(oldestAt) {
				oldest, oldestAt = id, at
			}
		}
		if oldest == "" {
			break
		}
		v.dropLocked(oldest)
	}
}
