package guard

import (
	"container/list"
	"crypto/rand"
	"fmt"
	"log/slog"
	"math/big"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/polarisagi/polaris/internal/observability/metrics"
)

// piiMappingMaxEntries 单个分区保留的原值→假值映射上限。
// 超限按 LRU 淘汰：被淘汰的原值再次出现会得到新的假值，一致性仅在窗口内保证。
// 取值依据：单条映射约 64B（两个短字符串 + map/list 开销），10000 条 ≈ 640KB，
// 对 Tier-0 2GB VPS 可接受。阈值 SSoT 登记见阶段06 docs/arch/spec/state.yaml。
const piiMappingMaxEntries = 10000

// piiPartitionMaxEntries 同时保留的分区（通常 = SessionID）数上限，超限按
// LRU 淘汰整个分区（连同其全部 original→fake 映射一并回收）。
const piiPartitionMaxEntries = 256

// piiGlobalPartition 调用方无法提供 SessionID/NamespaceID 时的兜底分区。
// 落在这个分区里的映射仍受 piiMappingMaxEntries 约束，只是不能被
// ReleasePartition 精确回收（因为没有可用的分区键），依赖 LRU 兜底防 OOM。
const piiGlobalPartition = "global"

// evictLogSampleN 淘汰事件的日志采样率：每 N 次淘汰打一条 Warn，避免长会话
// 持续淘汰时刷屏；计数器 metrics.RecordPIIMappingEviction 每次都记录，不采样。
const evictLogSampleN = 100

// lruEntry 是 lruMapping 内部 container/list 节点承载的数据。
type lruEntry struct {
	key   string
	value string
}

// lruMapping 单分区内 original→fake 的有界 LRU 映射，线程安全。
type lruMapping struct {
	mu       sync.Mutex
	ll       *list.List // front = 最近使用
	elements map[string]*list.Element
}

func newLRUMapping() *lruMapping {
	return &lruMapping{
		ll:       list.New(),
		elements: make(map[string]*list.Element),
	}
}

// get 命中则将该条目移至队首（标记为最近使用）。
func (m *lruMapping) get(key string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if el, ok := m.elements[key]; ok {
		m.ll.MoveToFront(el)
		return el.Value.(*lruEntry).value, true
	}
	return "", false
}

// setIfAbsent 仅在 key 不存在时写入（避免并发场景下覆盖已生成的假值，
// 与旧实现的 double-check 语义等价）。返回最终生效的 value（可能是已存在的旧值）
// 与是否发生了淘汰。
func (m *lruMapping) setIfAbsent(key, value string) (final string, evicted bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if el, ok := m.elements[key]; ok {
		m.ll.MoveToFront(el)
		return el.Value.(*lruEntry).value, false
	}
	el := m.ll.PushFront(&lruEntry{key: key, value: value})
	m.elements[key] = el
	if m.ll.Len() > piiMappingMaxEntries {
		oldest := m.ll.Back()
		if oldest != nil {
			m.ll.Remove(oldest)
			delete(m.elements, oldest.Value.(*lruEntry).key)
			evicted = true
		}
	}
	return value, evicted
}

// PIIDesensitizer 格式保留假数据脱敏器。
// 映射按 partitionKey（推荐取 SessionID/NamespaceID）隔离：
//   - 同分区内，同一原值始终映射到同一假值（一致性承诺仅在分区内保证）；
//   - 不同分区的同一原值映射到不同假值，避免跨会话假值串号；
//   - Agent 会话终态时调用 ReleasePartition 可确定性回收整段内存，
//     LRU 仅作为兜底防 OOM（不能替代确定性清理，否则同分区内会因 LRU
//     淘汰破坏"同一原值同一假值"的一致性承诺 — 见阶段03 R-02 设计说明）。
type PIIDesensitizer struct {
	mu             sync.Mutex // 保护 partitions 与 partitionOrder（分区级 LRU）
	partitions     map[string]*lruMapping
	partitionOrder *list.List // front = 最近访问的分区
	partitionElems map[string]*list.Element

	evictLogCounter atomic.Uint64 // 采样计数器，与 evictLogSampleN 配合
}

func NewPIIDesensitizer() *PIIDesensitizer {
	return &PIIDesensitizer{
		partitions:     make(map[string]*lruMapping),
		partitionOrder: list.New(),
		partitionElems: make(map[string]*list.Element),
	}
}

// partitionFor 返回 partitionKey 对应的 lruMapping，不存在则创建；同时
// 维护分区级 LRU（触碰或新建都算一次访问），超限淘汰最久未访问的分区。
func (d *PIIDesensitizer) partitionFor(partitionKey string) *lruMapping {
	if partitionKey == "" {
		partitionKey = piiGlobalPartition
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	if el, ok := d.partitionElems[partitionKey]; ok {
		d.partitionOrder.MoveToFront(el)
		return d.partitions[partitionKey]
	}

	mapping := newLRUMapping()
	d.partitions[partitionKey] = mapping
	el := d.partitionOrder.PushFront(partitionKey)
	d.partitionElems[partitionKey] = el

	if d.partitionOrder.Len() > piiPartitionMaxEntries {
		oldest := d.partitionOrder.Back()
		if oldest != nil {
			oldestKey := oldest.Value.(string)
			d.partitionOrder.Remove(oldest)
			delete(d.partitionElems, oldestKey)
			delete(d.partitions, oldestKey)
			slog.Warn("guard/pii_desensitizer: 分区数超限，LRU 淘汰最久未访问分区（该分区内一致性承诺失效）", "evicted_partition", oldestKey, "max_partitions", piiPartitionMaxEntries)
		}
	}
	return mapping
}

// Desensitize 将原始值转换为同格式假数据，落在 piiGlobalPartition 兜底分区。
// 保留旧签名供未接线调用点过渡使用；新调用点应优先用 DesensitizeIn 并传入
// 真实 partitionKey（通常是 SessionID）。
func (d *PIIDesensitizer) Desensitize(piiType, original string) string {
	return d.DesensitizeIn(piiGlobalPartition, piiType, original)
}

// DesensitizeIn 在指定分区内将原始值转换为同格式假数据。
func (d *PIIDesensitizer) DesensitizeIn(partitionKey, piiType, original string) string {
	mapping := d.partitionFor(partitionKey)

	if fake, ok := mapping.get(original); ok {
		return fake
	}

	fake := generateFakeValue(piiType, original)

	final, evicted := mapping.setIfAbsent(original, fake)
	if evicted {
		// 不用 partitionKey 作 label：分区键通常取自 SessionID，属无界基数，
		// 违反 CardinalityGuard（见 docs/specs/09-LLM-Agent-Production.md）。
		// partition 信息仅进日志字段（下方采样 Warn），不进指标维度。
		metrics.RecordPIIMappingEviction()
		if n := d.evictLogCounter.Add(1); n%evictLogSampleN == 1 {
			slog.Warn("guard/pii_desensitizer: 分区内映射超限，LRU 淘汰最久未使用条目（采样日志）", "partition", partitionKey, "max_entries", piiMappingMaxEntries, "sampled_1_of", evictLogSampleN)
		}
	}
	return final
}

// ReleasePartition 确定性回收指定分区的全部映射内存。应在 Agent 会话终态
// （AgentStateComplete/AgentStateFailed）调用，而不是依赖 LRU 兜底——LRU
// 淘汰只防 OOM，不代表"这个会话已经结束、映射不再需要"。
func (d *PIIDesensitizer) ReleasePartition(partitionKey string) {
	if partitionKey == "" {
		return // 不回收兜底分区：其它未接线调用点可能仍在使用
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if el, ok := d.partitionElems[partitionKey]; ok {
		d.partitionOrder.Remove(el)
		delete(d.partitionElems, partitionKey)
	}
	delete(d.partitions, partitionKey)
}

// Clear 全量清空所有分区。供测试与 KillSwitch 恢复路径使用；生产主路径应
// 使用 ReleasePartition 做精确回收。
func (d *PIIDesensitizer) Clear() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.partitions = make(map[string]*lruMapping)
	d.partitionOrder = list.New()
	d.partitionElems = make(map[string]*list.Element)
}

func generateFakeValue(piiType, original string) string {
	switch piiType {
	case "email":
		return generateFakeEmail()
	case "phone_cn":
		return generateFakePhoneCN(original)
	case "phone_intl":
		return generateFakePhoneIntl(original)
	case "id_card_cn":
		return generateFakeIDCard()
	case "credit_card":
		return generateFakeCreditCard()
	case "ip":
		return generateFakeIP(original)
	default:
		// 兜底：如果是不识别的类型或 presidio type，生成长度相近的不可读字符或简单替换
		return fmt.Sprintf("REDACTED-%s", randomHex(4))
	}
}

func randomHex(n int) string {
	return secureRandomHex(n)
}

func generateFakeEmail() string {
	return fmt.Sprintf("test-%s@example.com", randomHex(4))
}

func generateFakePhoneCN(orig string) string {
	// 保留前3位，如果带 +86 则保留前面部分
	orig = strings.TrimSpace(orig)
	prefixLen := 3
	if strings.HasPrefix(orig, "+86") {
		prefixLen = 6 // +86139
	} else if strings.HasPrefix(orig, "0") {
		prefixLen = 4 // 0139
	}

	if len(orig) <= prefixLen {
		return orig
	}

	prefix := orig[:prefixLen]
	// 中国手机号是 11 位（不含前缀）。这里假设后8位随机生成
	var suffix string
	for i := 0; i < len(orig)-prefixLen; i++ {
		n, _ := rand.Int(rand.Reader, big.NewInt(10))
		suffix += n.String()
	}
	return prefix + suffix
}

func generateFakePhoneIntl(orig string) string {
	orig = strings.TrimSpace(orig)
	if !strings.HasPrefix(orig, "+") {
		return orig
	}

	// 保留前3个字符(如 +1, +44, +33)
	prefixLen := 3
	if len(orig) <= prefixLen {
		return orig
	}
	prefix := orig[:prefixLen]

	var suffix string
	for i := 0; i < len(orig)-prefixLen; i++ {
		n, _ := rand.Int(rand.Reader, big.NewInt(10))
		suffix += n.String()
	}
	return prefix + suffix
}

func generateFakeIDCard() string {
	// 避免使用真实存在的前缀。使用 999999 作为不可能存在的地区码。
	region := "999999"
	// 出生日期: 19900101
	birthday := "19900101"

	// 顺序码
	seqB := secureRandomBytes(1)
	seqInt := int(seqB[0]) % 1000
	seq := fmt.Sprintf("%03d", seqInt)

	base := region + birthday + seq

	// 计算校验位
	weight := []int{7, 9, 10, 5, 8, 4, 2, 1, 6, 3, 7, 9, 10, 5, 8, 4, 2}
	checkCode := []byte{'1', '0', 'X', '9', '8', '7', '6', '5', '4', '3', '2'}

	sum := 0
	for i := 0; i < 17; i++ {
		num, _ := strconv.Atoi(string(base[i]))
		sum += num * weight[i]
	}

	mod := sum % 11
	return base + string(checkCode[mod])
}

func generateFakeCreditCard() string {
	// 411111111111111 - 15 chars base for test Visa
	base := "411111111111111"

	sum := 0
	for i := 0; i < len(base); i++ {
		digit := int(base[i] - '0')
		if i%2 == 0 {
			digit *= 2
			if digit > 9 {
				digit -= 9
			}
		}
		sum += digit
	}

	check := (10 - (sum % 10)) % 10
	return base + strconv.Itoa(check)
}

func generateFakeIP(orig string) string {
	// 替换为 RFC 5737 预留网段 198.51.100.X
	// 对于 IPv6 可以考虑 RFC 3849 预留 2001:DB8::/32
	if strings.Contains(orig, ":") {
		// IPv6
		return "2001:db8::1234:5678"
	}
	// IPv4
	n, _ := rand.Int(rand.Reader, big.NewInt(254))
	return fmt.Sprintf("198.51.100.%d", n.Int64()+1)
}
