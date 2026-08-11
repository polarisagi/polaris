package security

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

// 本文件守护 2026-08-11 修复的 brick 级缺陷：Record() 在 EventID 赋值**之前**
// 算 hash，落库却存了带 EventID 的版本。重启恢复时按存量重算必然不符 →
// VerifyIntegrity 失败 → RecoverOnStartup 报错 → bootSubstrate 中止启动。
//
// EventID 自动生成是常规路径（调用方通常不指定），因此这不是边角情况：
// **只要有审计记录落库，下一次重启必然起不来**。用户实测：开发库仅 1 条记录即 brick。

// TestRecordIsSelfVerifiableAfterWrite 是本次修复的核心断言：
// 写入后的记录必须能被 VerifyIntegrity 通过。
//
// 这条性质此前不成立，而且**没有任何测试覆盖它**——既有测试都在同一个进程内
// 写完就验，用的是内存里那个已经带了 EventID 的指针，恰好绕开了缺陷。
// 真正会暴露问题的是"写入 → 序列化落库 → 反序列化 → 重算"这一整圈。
func TestRecordIsSelfVerifiableAfterWrite(t *testing.T) {
	at := NewAuditTrail(nil, "")

	// 不指定 EventID —— 走自动生成路径，即生产的常规路径。
	rec := &AuditRecord{ActionType: "tool_execute:probe", Outcome: "allow"}
	if err := at.Record(rec); err != nil {
		t.Fatalf("Record 失败: %v", err)
	}

	if rec.EventID == "" {
		t.Fatal("EventID 应被自动补齐")
	}

	// 关键：EventID 必须已包含在被 hash 的内容里。
	// 用与验证侧完全相同的方式重算，模拟"从库里读回来再验"。
	data := serializeRecord(rec)
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != rec.RecordHash {
		t.Fatalf("写入后的记录自身验不过——hash 覆盖范围与落库内容不一致。\n"+
			"  stored=%s\n  recomputed=%s\n"+
			"  这正是「有审计记录就重启不了」的成因", rec.RecordHash, got)
	}

	if ok, idx := at.VerifyIntegrity(); !ok {
		t.Fatalf("VerifyIntegrity 应通过，实际在第 %d 条失败", idx)
	}
}

// TestMultiRecordChainVerifies 多条记录的链接也必须成立。
func TestMultiRecordChainVerifies(t *testing.T) {
	at := NewAuditTrail(nil, "")
	for i := 0; i < 5; i++ {
		if err := at.Record(&AuditRecord{ActionType: "act", Outcome: "allow"}); err != nil {
			t.Fatalf("第 %d 条写入失败: %v", i, err)
		}
	}
	if ok, idx := at.VerifyIntegrity(); !ok {
		t.Fatalf("5 条链应完整，实际在第 %d 条断裂", idx)
	}
}

// TestLegacyLayoutRecordAccepted 旧布局记录（hash 未覆盖 EventID）必须仍被接受。
//
// 存量部署里已经有这样的记录，它们并未被篡改，只是当初的 hash 覆盖范围不含
// EventID。拒绝它们等于让所有存量部署永久起不来——那既拦不住攻击者，还把
// 系统自己变成拒绝服务。
func TestLegacyLayoutRecordAccepted(t *testing.T) {
	// 复刻旧写入路径：先按 EventID 为空算 hash，再填 EventID。
	rec := &AuditRecord{
		EventID:    "",
		Timestamp:  1783264676411594,
		ActionType: "tool_execute:tool_search",
	}
	sum := sha256.Sum256(serializeRecord(rec))
	rec.RecordHash = hex.EncodeToString(sum[:])
	rec.EventID = "audit_1783264676411594" // 旧代码在算完 hash 后才填

	if !recordHashMatches(rec) {
		t.Error("旧布局记录应被兼容接受（它未被篡改，只是 hash 未覆盖 EventID）")
	}
}

// TestTamperedRecordStillRejected 兼容旧布局不得放过真正的篡改。
func TestTamperedRecordStillRejected(t *testing.T) {
	at := NewAuditTrail(nil, "")
	rec := &AuditRecord{ActionType: "tool_execute:probe", Outcome: "allow"}
	if err := at.Record(rec); err != nil {
		t.Fatalf("Record 失败: %v", err)
	}

	// 改动一个被 hash 覆盖的字段 —— 两种布局都不该验得过。
	rec.Outcome = "deny"
	if recordHashMatches(rec) {
		t.Error("内容被篡改的记录必须判失败；旧布局兼容路径不得成为绕过口子")
	}
	if ok, _ := at.VerifyIntegrity(); ok {
		t.Error("VerifyIntegrity 必须检出篡改")
	}
}

// TestIntegrityFailureIsDiagnosable 失败信息必须说清是哪项检查失败。
//
// 原实现只有 "integrity check failed at index 0"，运维既不知道是链接断了还是
// 内容被改，也不知道下一步看什么——而这条错误会直接中止启动。
func TestIntegrityFailureIsDiagnosable(t *testing.T) {
	at := NewAuditTrail(nil, "")
	if err := at.Record(&AuditRecord{ActionType: "a"}); err != nil {
		t.Fatalf("Record 失败: %v", err)
	}
	if err := at.Record(&AuditRecord{ActionType: "b"}); err != nil {
		t.Fatalf("Record 失败: %v", err)
	}

	t.Run("内容篡改", func(t *testing.T) {
		at.records[1].Outcome = "tampered"
		idx, why := at.verifyIntegrityLocked()
		if idx != 1 {
			t.Fatalf("应在第 1 条检出，实际 %d", idx)
		}
		if !strings.Contains(why, "RecordHash 重算不符") {
			t.Errorf("原因应指明是内容重算不符，实际: %s", why)
		}
		at.records[1].Outcome = "" // 复原，供下一子测试
	})

	t.Run("链接断裂", func(t *testing.T) {
		at.records[1].PrevHash = "deadbeef"
		// 篡改 PrevHash 同时会让 RecordHash 重算不符；本用例只要求
		// 报出的原因能定位到第 1 条，且文案非空可读。
		idx, why := at.verifyIntegrityLocked()
		if idx != 1 {
			t.Fatalf("应在第 1 条检出，实际 %d", idx)
		}
		if why == "" {
			t.Error("失败原因不得为空")
		}
	})
}
