package security

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// 审计链完整性校验（R7 行数治理，2026-08-11 自 audit_trail.go 拆出）。
//
// 与 audit_trail.go 的分工：那边负责「写入与恢复」，这里负责「判定一条链是否
// 可信」。拆分点选在这里是因为二者的失效模式完全不同——写入路径的 bug 表现为
// 数据落不进去，校验路径的 bug 表现为**明明没问题却拒绝启动**（或反过来，
// 被篡改却放行）。本次 brick 事故正是二者交界处的缺陷：写入侧在 hash 之后才
// 补 EventID，校验侧照实重算自然对不上。

// VerifyIntegrity 遍历 hash chain 验证完整性。
// 发现断裂返回 (false, brokenIndex)；完整返回 (true, -1)。
func (at *AuditTrail) VerifyIntegrity() (bool, int) {
	at.mu.RLock()
	defer at.mu.RUnlock()

	broken, _ := at.verifyIntegrityLocked()
	if broken >= 0 {
		return false, broken
	}
	return true, -1
}

// verifyIntegrityLocked 返回 (首个断裂下标, 断裂原因)；完整时返回 (-1, "")。
// 需持读锁调用。
//
// 拆出独立函数是为了把**断裂原因**带出来。原实现只返回下标，
// "integrity check failed at index 0" 无法区分是链接断了还是内容被改，
// 而这两者的处置完全不同（前者指向记录缺失/乱序，后者指向内容篡改）。
func (at *AuditTrail) verifyIntegrityLocked() (int, string) {
	for i, r := range at.records {
		// 检查 PrevHash 链接
		if i > 0 {
			if r.PrevHash != at.records[i-1].RecordHash {
				return i, fmt.Sprintf("PrevHash 链接断裂：本条 PrevHash=%s，上一条 RecordHash=%s",
					shortHash(r.PrevHash), shortHash(at.records[i-1].RecordHash))
			}
		} else if at.epochStartHash != "" {
			if r.PrevHash != at.epochStartHash {
				return 0, fmt.Sprintf("首条 PrevHash 与截断锚点不符：PrevHash=%s，锚点=%s",
					shortHash(r.PrevHash), shortHash(at.epochStartHash))
			}
		}
		// 重算 RecordHash 校验数据未被篡改
		if recordHashMatches(r) {
			continue
		}
		return i, fmt.Sprintf("RecordHash 重算不符（记录内容与其 hash 不一致）：event_id=%s stored=%s",
			r.EventID, shortHash(r.RecordHash))
	}
	return -1, ""
}

// recordHashMatches 校验单条记录的 RecordHash，兼容 2026-08-11 之前的旧布局。
//
// 旧布局的成因见 Record() 内注释：hash 算在 EventID 赋值之前，落库却存了带
// EventID 的版本。对这些存量记录，用当前方式重算必然不符——但它们**并未被篡改**，
// 只是当初的 hash 覆盖范围不含 EventID。
//
// 处置：先按当前布局验；不符再按旧布局（EventID 置空）验一次。旧布局命中即认为
// 记录真实，代价是这些记录的 EventID 字段未被 hash 覆盖（攻击者可在不被发现的
// 前提下改动**旧记录**的 EventID）。这是一个有界的减弱，换掉的是"所有存量部署
// 一重启就永久起不来"——后者既拦不住攻击者，还把系统本身变成拒绝服务。
// 新写入的记录一律走新布局，该兼容路径随存量记录被归档而自然消亡。
func recordHashMatches(r *AuditRecord) bool {
	data := serializeRecord(r)
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) == r.RecordHash {
		return true
	}
	if r.EventID == "" {
		return false // 无 EventID 可清空，旧布局与新布局等价，无需重试
	}
	legacy := *r
	legacy.EventID = ""
	legacySum := sha256.Sum256(serializeRecord(&legacy))
	return hex.EncodeToString(legacySum[:]) == r.RecordHash
}

// shortHash 截断 hash 便于阅读；空值显式标注而非留空，避免日志里出现
// "PrevHash=，上一条=abc" 这种看不出是空还是缺字段的输出。
func shortHash(h string) string {
	if h == "" {
		return "(空)"
	}
	if len(h) <= 12 {
		return h
	}
	return h[:12] + "…"
}
