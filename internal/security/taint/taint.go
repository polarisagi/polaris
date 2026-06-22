package taint

import (
	stdhmac "crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/polarisagi/polaris/pkg/types"
)

// Taint Tracking — 污点追踪类型系统。
// 权威 TaintLevel 枚举定义见 internal/protocol/types.go。
// 架构文档: docs/arch/11-Policy-Safety-深度选型.md §2

// TaintedString 是带污点标记的字符串。
// content 未导出——仅 Sanitize() 可构造 SafeString。
// PromptBuilder.WriteInstruction 仅接受 SafeString。
type TaintedString struct {
	content string
	Source  TaintSource
	Origin  string
}

// SafeString 是经清洗的安全字符串，仅 Sanitize() 可构造。
type SafeString struct {
	content string
}

// TaintSource 记录污点来源。
type TaintSource struct {
	Module           string
	EntityID         string
	EventID          string
	OriginTaintLevel types.TaintLevel
}

// NewTaintedString 创建一个带有安全污点标记的字符串。
// 默认情况下，只要被包裹，外部就无法轻易将它作为普通字符串拼接到 Prompt 中。
func NewTaintedString(content string, source TaintSource, origin string) TaintedString {
	return TaintedString{
		content: content,
		Source:  source,
		Origin:  origin,
	}
}

// Content 获取受污染的原始内容。
// 注意：只应在明确不需要安全清洗的场景下使用此方法（如：写入数据库、发送到受限沙箱的数据槽）。
func (ts TaintedString) Content() string {
	return ts.content
}

// Level 获取当前的污点等级。
func (ts TaintedString) Level() types.TaintLevel {
	return ts.Source.OriginTaintLevel
}

// IsEmpty 报告内容是否为空字符串，无需暴露原始内容。
// 用于替代 .Content() != "" 的空值检查，减少裸 .Content() 调用点。
func (ts TaintedString) IsEmpty() bool {
	return ts.content == ""
}

// Fields 将内容按 Unicode 空白分词（等价于 strings.Fields），供词元化使用。
// 调用方无需提取原始字符串即可完成 SurpriseIndex 等计算。
func (ts TaintedString) Fields() []string {
	return strings.Fields(ts.content)
}

// MarshalJSONString 将内容 JSON 序列化为带引号的字符串字面量（如 `"hello"`）。
// 专为拼接 FastPath JSON payload 设计，避免调用方出现裸 .Content() + json.Marshal 组合。
func (ts TaintedString) MarshalJSONString() string {
	b, _ := json.Marshal(ts.content)
	return string(b)
}

// AppendToMap 若内容非空，将内容以 key 写入 m，返回是否写入。
// 专为向安全存储（PII Vault、kv map）传递污点值设计，避免调用方出现裸 .Content() 提取。
func (ts TaintedString) AppendToMap(m map[string]string, key string) bool {
	if ts.content == "" {
		return false
	}
	m[key] = ts.content
	return true
}

// Content 获取已清洗的绝对安全字符串。
// 此字符串可以安全地注入到 LLM 的 Instruction Slot 中。
func (ss SafeString) Content() string {
	return ss.content
}

// IntoMessage 将安全字符串直接构造为指定角色的 types.Message。
// 供 PromptBuilder 使用，避免调用方在 policy 豁免域外出现 .Content() 调用。
func (ss SafeString) IntoMessage(role string) types.Message {
	return types.Message{Role: role, Content: ss.content}
}

// =============================================================================
// TaintTracker — 运行时污点传播追踪器（M11 §2.1 第一重防护）
// 追踪每个字符串 ID 的污点等级，GetMaxTaint 实现 PropagateTaint 语义：
// output = max(所有输入的 TaintLevel)，只升不降。
// =============================================================================

// TaintTracker 线程安全的运行时污点追踪器。
type TaintTracker struct {
	mu     sync.RWMutex
	levels map[string]types.TaintLevel // id → TaintLevel
}

// NewTaintTracker 创建新的追踪器实例。
func NewTaintTracker() *TaintTracker {
	return &TaintTracker{
		levels: make(map[string]types.TaintLevel),
	}
}

// Track 记录字符串 ID 的污点等级。
// 遵循单调不递减原则：只能升级，不能降级。
func (tt *TaintTracker) Track(id string, level types.TaintLevel) {
	tt.mu.Lock()
	defer tt.mu.Unlock()
	if existing, ok := tt.levels[id]; !ok || level > existing {
		tt.levels[id] = level
	}
}

// GetLevel 获取单个 ID 的当前污点等级。
func (tt *TaintTracker) GetLevel(id string) types.TaintLevel {
	tt.mu.RLock()
	defer tt.mu.RUnlock()
	return tt.levels[id]
}

// GetMaxTaint 实现 PropagateTaint 语义：返回所有指定 ID 中最高的污点等级。
// 用于合并多个输入的污点，决定输出的最终污点等级。
func (tt *TaintTracker) GetMaxTaint(ids ...string) types.TaintLevel {
	tt.mu.RLock()
	defer tt.mu.RUnlock()
	var max types.TaintLevel
	for _, id := range ids {
		if l, ok := tt.levels[id]; ok && l > max {
			max = l
		}
	}
	return max
}

// =============================================================================
// Spotlighting — 不可信数据围栏标记（M11 §2.2）
// 步骤1: 生成标记 = SHA-256(content)[:8]（内容派生，保证重放确定性）
// 步骤2: 包裹为 "=== UNTRUSTED_DATA_{hex} ===\n{data}\n=== END_UNTRUSTED_DATA ==="
// 调用方: kernel.PromptBuilder.WriteUserData
// =============================================================================

// Spotlighting 对不可信数据槽内容加围栏标记，防止 LLM 将其解析为系统指令。
// 仅适用于 TaintMedium 及以上的内容；TaintLow/TaintNone 无需包裹。
func Spotlighting(ts TaintedString) string {
	if ts.Source.OriginTaintLevel < types.TaintMedium {
		return ts.content
	}
	hash := sha256.Sum256([]byte(ts.content))
	marker := hex.EncodeToString(hash[:])[:8]
	return fmt.Sprintf("=== UNTRUSTED_DATA_%s ===\n%s\n=== END_UNTRUSTED_DATA ===", marker, ts.content)
}

// =============================================================================
// TaintBoundary — 跨模块边界 HMAC 验证（inv_M11_02）
// 防止反序列化路径绕过污点标记：序列化时附加 HMAC-SHA256，
// 反序列化时重新计算并对比，不匹配则强制升级到 TaintHigh。
// key 由调用方从 Capability token.Token 派生（或使用共享密钥），不存储于负载中。
// =============================================================================

// TaintBoundarySerializer 跨边界污点序列化器。
type TaintBoundarySerializer struct {
	key []byte // HMAC-SHA256 密钥（由调用方从 CapToken 派生）
}

// NewTaintBoundarySerializer 创建序列化器。key 不得为空（fail-fast）。
func NewTaintBoundarySerializer(key []byte) *TaintBoundarySerializer {
	if len(key) == 0 {
		panic("taint_boundary: HMAC key must not be empty")
	}
	return &TaintBoundarySerializer{key: key}
}

// TaintEnvelope 跨边界传输的污点信封。
// HMACHex 覆盖全信封（除 hmac 字段自身外），防止部分字段被篡改。
type TaintEnvelope struct {
	Content string           `json:"content"`
	Level   types.TaintLevel `json:"level"`
	Source  TaintSource      `json:"source"`
	HMACHex string           `json:"hmac"`
}

// taintEnvelopeForMAC 是用于 HMAC 计算的信封副本（不含 hmac 字段），
// 保证序列化字段集合与 TaintEnvelope 完全一致，防止字段遗漏。
type taintEnvelopeForMAC struct {
	Content string           `json:"content"`
	Level   types.TaintLevel `json:"level"`
	Source  TaintSource      `json:"source"`
}

// Seal 序列化 TaintedString 为带 HMAC 的信封（传输至另一模块前调用）。
func (s *TaintBoundarySerializer) Seal(ts TaintedString) TaintEnvelope {
	env := TaintEnvelope{
		Content: ts.content,
		Level:   ts.Source.OriginTaintLevel,
		Source:  ts.Source,
	}
	env.HMACHex = s.computeHMAC(env)
	return env
}

// Unseal 反序列化信封并以常量时间验证 HMAC 完整性。
// 若 HMAC 不匹配，返回的 TaintedString 污点强制升级为 TaintHigh（fail-closed）。
func (s *TaintBoundarySerializer) Unseal(env TaintEnvelope) (TaintedString, bool) {
	expectedHex := s.computeHMAC(env)

	// 常量时间比较，防止时序攻击（timing attack）
	expectedBytes, err1 := hex.DecodeString(expectedHex)
	receivedBytes, err2 := hex.DecodeString(env.HMACHex)
	valid := err1 == nil && err2 == nil && stdhmac.Equal(expectedBytes, receivedBytes)

	if !valid {
		src := env.Source
		src.OriginTaintLevel = types.TaintHigh
		return TaintedString{
			content: env.Content,
			Source:  src,
			Origin:  "hmac_mismatch_upgraded",
		}, false
	}
	return TaintedString{
		content: env.Content,
		Source:  env.Source,
		Origin:  env.Source.EntityID,
	}, true
}

// computeHMAC 对完整信封（Content + Level + Source 全部字段）计算 HMAC-SHA256。
// 使用规范化 JSON 序列化覆盖全部字段，防止部分字段篡改绕过校验。
// 采用标准库 crypto/hmac + sha256.New，正确处理长密钥（HMAC 规范要求预哈希）。
func (s *TaintBoundarySerializer) computeHMAC(env TaintEnvelope) string {
	// 序列化不含 hmac 字段的副本，保证确定性（json.Marshal 字段顺序由结构体定义固定）
	canonical, _ := json.Marshal(taintEnvelopeForMAC{
		Content: env.Content,
		Level:   env.Level,
		Source:  env.Source,
	})
	mac := stdhmac.New(sha256.New, s.key)
	mac.Write(canonical)
	return hex.EncodeToString(mac.Sum(nil))
}
