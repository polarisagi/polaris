package builtin

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/memory/store"
	"github.com/polarisagi/polaris/internal/memory/testutil"
	"github.com/polarisagi/polaris/pkg/types"
)

// TestMakeMemoryExpireFn_MarksEntityExpired 验证本轮审查修复的核心验收点：
// memory_expire 工具调用后，目标实体的 status 变为 'expired'（依赖 WHERE
// status='active' 过滤，见 004_semantic_memory.sql，后续检索不会再命中该实体）。
//
// 复核背景：修复前 MakeMemoryExpireFn 调用 writer.Archive(ctx, ent.ID, reason)，
// 但 Archive 操作 KV store 的 "doc:"+id 命名空间（面向 types.Document），而
// ent.ID 是 GetEntity 返回的 "entity:"+DBID（SQL 表主键）——两套地址空间不
// 重叠，Archive 对实体场景必然 not-found，memory_expire 从未真正生效，且此前
// 完全没有测试覆盖到这一路径。现改为调用 MarkEntityExpired 直接操作
// semantic_entities 表，此测试验证修复后的行为。
func TestMakeMemoryExpireFn_MarksEntityExpired(t *testing.T) {
	ctx := context.Background()
	mockStore := testutil.NewMockStore()
	sm := store.NewSemanticMem(mockStore, &testutil.MockIntentSubmitter{})

	entity := types.Entity{
		Name:       "outdated-fact",
		Type:       "Fact",
		Properties: map[string]any{"note": "no longer true"},
		Version:    int(time.Now().UnixNano()),
	}
	if err := sm.UpsertFact(ctx, entity, types.TaintNone); err != nil {
		t.Fatalf("UpsertFact failed: %v", err)
	}

	before, err := sm.GetEntity(ctx, "Fact", "outdated-fact")
	if err != nil {
		t.Fatalf("GetEntity (before) failed: %v", err)
	}
	if before.Status != "active" {
		t.Fatalf("expected freshly written entity to be active, got %q", before.Status)
	}

	fn := MakeMemoryExpireFn(sm)
	args := memoryExpireArgs{EntityType: "Fact", Name: "outdated-fact", Reason: "user requested forget"}
	argsJSON, _ := json.Marshal(args)

	out, err := fn(ctx, argsJSON)
	if err != nil {
		t.Fatalf("MakeMemoryExpireFn failed: %v", err)
	}
	var resp map[string]string
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp["status"] != "success" {
		t.Fatalf("expected status=success, got %q (full resp: %s)", resp["status"], out)
	}

	after, err := sm.GetEntity(ctx, "Fact", "outdated-fact")
	if err != nil {
		t.Fatalf("GetEntity (after) failed: %v", err)
	}
	if after.Status != "expired" {
		t.Errorf("expected status=expired after memory_expire, got %q", after.Status)
	}
}

// TestMakeMemoryExpireFn_NotFound 验证目标实体不存在时返回软失败 JSON（不 panic、
// 不返回 Go error），因为"实体不存在"是预期场景而非错误。
func TestMakeMemoryExpireFn_NotFound(t *testing.T) {
	ctx := context.Background()
	sm := store.NewSemanticMem(testutil.NewMockStore(), &testutil.MockIntentSubmitter{})

	fn := MakeMemoryExpireFn(sm)
	args := memoryExpireArgs{EntityType: "Fact", Name: "never-existed"}
	argsJSON, _ := json.Marshal(args)

	out, err := fn(ctx, argsJSON)
	if err != nil {
		t.Fatalf("expected soft-fail (no Go error) for missing entity, got err: %v", err)
	}
	var resp map[string]string
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp["status"] != "not_found" {
		t.Errorf("expected status=not_found, got %q", resp["status"])
	}
}

// TestMakeMemoryExpireFn_InvalidArgs 验证非法 JSON 输入返回错误而不 panic。
func TestMakeMemoryExpireFn_InvalidArgs(t *testing.T) {
	sm := store.NewSemanticMem(testutil.NewMockStore(), &testutil.MockIntentSubmitter{})
	fn := MakeMemoryExpireFn(sm)

	_, err := fn(context.Background(), []byte("not-json"))
	if err == nil {
		t.Fatal("expected error for invalid JSON args, got nil")
	}
}
