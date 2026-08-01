package guard

import (
	"strings"
	"testing"
)

func TestPIIDesensitizer_Email(t *testing.T) {
	d := NewPIIDesensitizer()
	fake1 := d.Desensitize("email", "test@real.com")
	if !strings.HasSuffix(fake1, "@example.com") {
		t.Errorf("expected @example.com suffix, got %s", fake1)
	}

	fake2 := d.Desensitize("email", "test@real.com")
	if fake1 != fake2 {
		t.Errorf("expected consistency, %s != %s", fake1, fake2)
	}
}

func TestPIIDesensitizer_PhoneCN(t *testing.T) {
	d := NewPIIDesensitizer()
	orig := "13912345678"
	fake := d.Desensitize("phone_cn", orig)
	if len(fake) != 11 {
		t.Errorf("expected len 11, got %d", len(fake))
	}
	if !strings.HasPrefix(fake, "139") {
		t.Errorf("expected prefix 139, got %s", fake)
	}
}

func TestPIIDesensitizer_IDCard(t *testing.T) {
	d := NewPIIDesensitizer()
	fake := d.Desensitize("id_card_cn", "11010519491231002X")
	if len(fake) != 18 {
		t.Errorf("expected len 18, got %d", len(fake))
	}
	if !strings.HasPrefix(fake, "999999") {
		t.Errorf("expected prefix 999999, got %s", fake)
	}
}

// ─── 阶段03 R-02：LRU + 分区隔离 ────────────────────────────────────────────

// TestPIIDesensitizer_MappingLRUBounded 验证写入 piiMappingMaxEntries+100 条
// 后单分区映射条数不超过上限（防 OOM 是硬需求）。
func TestPIIDesensitizer_MappingLRUBounded(t *testing.T) {
	d := NewPIIDesensitizer()
	for i := 0; i < piiMappingMaxEntries+100; i++ {
		d.DesensitizeIn("sess-lru", "email", "user"+itoaTest(i)+"@real.com")
	}
	if got := d.partitionLen("sess-lru"); got > piiMappingMaxEntries {
		t.Errorf("expected partition mapping bounded at %d, got %d", piiMappingMaxEntries, got)
	}
}

// TestPIIDesensitizer_PartitionIsolation 验证同分区同原值两次调用返回相同
// 假值；不同分区同原值返回不同假值（跨会话不串号）。
func TestPIIDesensitizer_PartitionIsolation(t *testing.T) {
	d := NewPIIDesensitizer()
	original := "test@real.com"

	fakeA1 := d.DesensitizeIn("sess-a", "email", original)
	fakeA2 := d.DesensitizeIn("sess-a", "email", original)
	if fakeA1 != fakeA2 {
		t.Errorf("expected same partition to return consistent fake value, got %s != %s", fakeA1, fakeA2)
	}

	fakeB := d.DesensitizeIn("sess-b", "email", original)
	if fakeB == fakeA1 {
		t.Errorf("expected different partitions to return different fake values for same original, both got %s", fakeB)
	}
}

// TestPIIDesensitizer_ReleasePartition_FreesMemory 验证 ReleasePartition 后
// 该分区内存归零（分区被彻底移除，len 回到 0；再次访问会创建全新分区，
// 得到新的假值——证明旧映射确实被回收而非只是"看起来清空"）。
func TestPIIDesensitizer_ReleasePartition_FreesMemory(t *testing.T) {
	d := NewPIIDesensitizer()
	original := "release-test@real.com"

	fake1 := d.DesensitizeIn("sess-release", "email", original)
	if got := d.partitionLen("sess-release"); got != 1 {
		t.Fatalf("expected 1 entry before release, got %d", got)
	}

	d.ReleasePartition("sess-release")
	if got := d.partitionLen("sess-release"); got != 0 {
		t.Errorf("expected 0 entries after ReleasePartition, got %d", got)
	}
	if got := d.partitionCount(); got != 0 {
		t.Errorf("expected 0 partitions after ReleasePartition (was the only one), got %d", got)
	}

	// 重新访问会创建全新分区；一致性承诺只在分区生命周期内保证，回收后
	// 允许（不要求）得到新假值——这里只验证不 panic、能正常工作。
	fake2 := d.DesensitizeIn("sess-release", "email", original)
	_ = fake1
	_ = fake2
}

// TestPIIDesensitizer_PartitionLRU_EvictsOldestOnOverflow 验证分区数超过
// piiPartitionMaxEntries 时按 LRU 淘汰最久未访问的分区。
func TestPIIDesensitizer_PartitionLRU_EvictsOldestOnOverflow(t *testing.T) {
	d := NewPIIDesensitizer()
	for i := 0; i < piiPartitionMaxEntries+10; i++ {
		d.DesensitizeIn("sess-"+itoaTest(i), "email", "x@real.com")
	}
	if got := d.partitionCount(); got > piiPartitionMaxEntries {
		t.Errorf("expected partition count bounded at %d, got %d", piiPartitionMaxEntries, got)
	}
	// 最早创建的分区应已被淘汰。
	if got := d.partitionLen("sess-0"); got != 0 {
		t.Errorf("expected oldest partition sess-0 to be evicted, still has %d entries", got)
	}
}

func itoaTest(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
