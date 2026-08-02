package builtin

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/memory/store"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/protocol/schema"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"

	_ "modernc.org/sqlite"
)

// newTestCoreMemoryStore 建一份使用 SSoT DDL（034_core_memory.sql）的真实 SQLite
// 实现，而非手工复制 schema 文本——避免阶段04 A-02 记录过的"内联 DDL 与 SSoT 失步"
// 故障重演（ADR-0082）。同时返回底层 *sql.DB，供 read_only 测试直接操纵表数据
// 模拟"系统已创建保护块"这一当前无生产写手的前置状态。
func newTestCoreMemoryStore(t *testing.T) (*store.SQLCoreMemoryStore, *sql.DB) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	ddl, err := schema.FS.ReadFile("034_core_memory.sql")
	require.NoError(t, err)
	_, err = db.Exec(string(ddl))
	require.NoError(t, err)

	return store.NewSQLCoreMemoryStore(db), db
}

func ctxWithTaint(agentID, sessionID string, taint types.TaintLevel) context.Context {
	ctx := ctxWithAgentSession(agentID, sessionID)
	return context.WithValue(ctx, protocol.CtxTaintLevelKey{}, taint)
}

func callCoreMemoryEdit(t *testing.T, fn func(ctx context.Context, input []byte) ([]byte, error), ctx context.Context, args map[string]any) ([]byte, error) {
	t.Helper()
	raw, err := json.Marshal(args)
	require.NoError(t, err)
	return fn(ctx, raw)
}

func TestCoreMemoryEdit_ListDoesNotReturnContent(t *testing.T) {
	cm, _ := newTestCoreMemoryStore(t)
	ctx := ctxWithTaint("agent-1", "sess-1", types.TaintNone)
	fn := MakeCoreMemoryEditFn(cm)

	_, err := callCoreMemoryEdit(t, fn, ctx, map[string]any{
		"operation": "set", "block_key": "persona", "content": "I am a helpful assistant.",
	})
	require.NoError(t, err)

	out, err := callCoreMemoryEdit(t, fn, ctx, map[string]any{"operation": "list"})
	require.NoError(t, err)
	assert.NotContains(t, string(out), "helpful assistant", "list 响应不得包含 content")

	var resp struct {
		Blocks []coreMemoryBlockSummary `json:"blocks"`
	}
	require.NoError(t, json.Unmarshal(out, &resp))
	require.Len(t, resp.Blocks, 1)
	assert.Equal(t, "persona", resp.Blocks[0].BlockKey)
	assert.Equal(t, len("I am a helpful assistant."), resp.Blocks[0].SizeBytes)
	assert.False(t, resp.Blocks[0].ReadOnly)
}

func TestCoreMemoryEdit_Replace(t *testing.T) {
	cm, _ := newTestCoreMemoryStore(t)
	ctx := ctxWithTaint("agent-1", "sess-1", types.TaintNone)
	fn := MakeCoreMemoryEditFn(cm)

	_, err := callCoreMemoryEdit(t, fn, ctx, map[string]any{
		"operation": "set", "block_key": "notes", "content": "alpha beta alpha",
	})
	require.NoError(t, err)

	t.Run("unique match succeeds", func(t *testing.T) {
		_, err := callCoreMemoryEdit(t, fn, ctx, map[string]any{
			"operation": "set", "block_key": "unique", "content": "only one gamma here",
		})
		require.NoError(t, err)
		out, err := callCoreMemoryEdit(t, fn, ctx, map[string]any{
			"operation": "replace", "block_key": "unique", "old_str": "gamma", "new_str": "delta",
		})
		require.NoError(t, err)
		assert.Contains(t, string(out), `"occurrences":1`)
		block, err := cm.Get(ctx, "agent-1", "sess-1", "unique")
		require.NoError(t, err)
		assert.Equal(t, "only one delta here", block.Content)
	})

	t.Run("zero matches -> NotFound", func(t *testing.T) {
		_, err := callCoreMemoryEdit(t, fn, ctx, map[string]any{
			"operation": "replace", "block_key": "notes", "old_str": "zzz-not-there", "new_str": "x",
		})
		require.Error(t, err)
		assert.Equal(t, apperr.CodeNotFound, apperr.CodeOf(err))
	})

	t.Run("ambiguous match without replace_all -> InvalidInput", func(t *testing.T) {
		_, err := callCoreMemoryEdit(t, fn, ctx, map[string]any{
			"operation": "replace", "block_key": "notes", "old_str": "alpha", "new_str": "X",
		})
		require.Error(t, err)
		assert.Equal(t, apperr.CodeInvalidInput, apperr.CodeOf(err))
		block, err := cm.Get(ctx, "agent-1", "sess-1", "notes")
		require.NoError(t, err)
		assert.Equal(t, "alpha beta alpha", block.Content, "内容不得被部分修改")
	})

	t.Run("replace_all replaces every occurrence", func(t *testing.T) {
		out, err := callCoreMemoryEdit(t, fn, ctx, map[string]any{
			"operation": "replace", "block_key": "notes", "old_str": "alpha", "new_str": "X", "replace_all": true,
		})
		require.NoError(t, err)
		assert.Contains(t, string(out), `"occurrences":2`)
		block, err := cm.Get(ctx, "agent-1", "sess-1", "notes")
		require.NoError(t, err)
		assert.Equal(t, "X beta X", block.Content)
	})
}

func TestCoreMemoryEdit_ReadOnlyBlocksRejectAllWrites(t *testing.T) {
	cm, db := newTestCoreMemoryStore(t)
	ctx := ctxWithTaint("agent-1", "sess-1", types.TaintNone)
	fn := MakeCoreMemoryEditFn(cm)

	_, err := callCoreMemoryEdit(t, fn, ctx, map[string]any{
		"operation": "set", "block_key": "protected", "content": "system constraint",
	})
	require.NoError(t, err)

	// 目前无生产侧写手创建保护块（ADR-0082 已登记留待后续接入），测试直接操作
	// 底层表模拟"系统已创建的保护块"这一前置状态。
	_, err = db.Exec(`UPDATE core_memory_blocks SET read_only = 1 WHERE block_key = 'protected'`)
	require.NoError(t, err)

	cases := []map[string]any{
		{"operation": "set", "block_key": "protected", "content": "override"},
		{"operation": "append", "block_key": "protected", "content": "more"},
		{"operation": "replace", "block_key": "protected", "old_str": "system", "new_str": "x"},
		{"operation": "delete", "block_key": "protected"},
	}
	for _, args := range cases {
		_, err := callCoreMemoryEdit(t, fn, ctx, args)
		require.Error(t, err, "operation %v should be forbidden", args["operation"])
		assert.Equal(t, apperr.CodeForbidden, apperr.CodeOf(err), "operation %v", args["operation"])
	}

	block, err := cm.Get(ctx, "agent-1", "sess-1", "protected")
	require.NoError(t, err)
	assert.Equal(t, "system constraint", block.Content, "受保护块内容不得被修改")
}

func TestCoreMemoryEdit_ExceedsMaxBytesRejectedUnchanged(t *testing.T) {
	cm, _ := newTestCoreMemoryStore(t)
	ctx := ctxWithTaint("agent-1", "sess-1", types.TaintNone)
	fn := MakeCoreMemoryEditFn(cm)

	limit := config.DefaultThresholds().M5Memory.CoreMemoryBlockMaxKB * 1024
	tooLarge := strings.Repeat("x", limit+1)

	_, err := callCoreMemoryEdit(t, fn, ctx, map[string]any{
		"operation": "set", "block_key": "big", "content": tooLarge,
	})
	require.Error(t, err)
	assert.Equal(t, apperr.CodeInvalidInput, apperr.CodeOf(err))
	assert.Contains(t, err.Error(), "exceeds limit")

	block, err := cm.Get(ctx, "agent-1", "sess-1", "big")
	require.NoError(t, err)
	assert.Nil(t, block, "超限写入不得创建任何行")
}

func TestCoreMemoryEdit_TaintOnlyUp(t *testing.T) {
	cm, _ := newTestCoreMemoryStore(t)
	fn := MakeCoreMemoryEditFn(cm)

	noneCtx := ctxWithTaint("agent-1", "sess-1", types.TaintNone)
	_, err := callCoreMemoryEdit(t, fn, noneCtx, map[string]any{
		"operation": "set", "block_key": "k", "content": "v1",
	})
	require.NoError(t, err)
	block, err := cm.Get(noneCtx, "agent-1", "sess-1", "k")
	require.NoError(t, err)
	assert.Equal(t, types.TaintNone, block.TaintLevel)

	highCtx := ctxWithTaint("agent-1", "sess-1", types.TaintHigh)
	_, err = callCoreMemoryEdit(t, fn, highCtx, map[string]any{
		"operation": "append", "block_key": "k", "content": "v2",
	})
	require.NoError(t, err)
	block, err = cm.Get(noneCtx, "agent-1", "sess-1", "k")
	require.NoError(t, err)
	assert.Equal(t, types.TaintHigh, block.TaintLevel, "append 高污点入参应上调块污点")

	// 反向：再以 TaintNone 调用 append，污点不得被下调。
	_, err = callCoreMemoryEdit(t, fn, noneCtx, map[string]any{
		"operation": "append", "block_key": "k", "content": "v3",
	})
	require.NoError(t, err)
	block, err = cm.Get(noneCtx, "agent-1", "sess-1", "k")
	require.NoError(t, err)
	assert.Equal(t, types.TaintHigh, block.TaintLevel, "污点只升不降")
}

func TestCoreMemoryEdit_BlockCountLimit(t *testing.T) {
	cm, _ := newTestCoreMemoryStore(t)
	ctx := ctxWithTaint("agent-1", "sess-1", types.TaintNone)
	fn := MakeCoreMemoryEditFn(cm)

	maxBlocks := config.DefaultThresholds().M5Memory.CoreMemoryMaxBlocks
	for i := 0; i < maxBlocks; i++ {
		_, err := callCoreMemoryEdit(t, fn, ctx, map[string]any{
			"operation": "set", "block_key": keyFor(i), "content": "v",
		})
		require.NoError(t, err, "block %d within limit should succeed", i)
	}

	_, err := callCoreMemoryEdit(t, fn, ctx, map[string]any{
		"operation": "set", "block_key": "one_too_many", "content": "v",
	})
	require.Error(t, err)
	assert.Equal(t, apperr.CodeResourceExhausted, apperr.CodeOf(err))

	// 已存在块的更新不受块数上限约束。
	_, err = callCoreMemoryEdit(t, fn, ctx, map[string]any{
		"operation": "set", "block_key": keyFor(0), "content": "v-updated",
	})
	assert.NoError(t, err)
}

func TestCoreMemoryEdit_DescribeAndGet(t *testing.T) {
	cm, _ := newTestCoreMemoryStore(t)
	ctx := ctxWithTaint("agent-1", "sess-1", types.TaintNone)
	fn := MakeCoreMemoryEditFn(cm)

	_, err := callCoreMemoryEdit(t, fn, ctx, map[string]any{
		"operation": "set", "block_key": "k", "content": "v",
	})
	require.NoError(t, err)

	_, err = callCoreMemoryEdit(t, fn, ctx, map[string]any{
		"operation": "describe", "block_key": "k", "description": "tracks task state",
	})
	require.NoError(t, err)

	out, err := callCoreMemoryEdit(t, fn, ctx, map[string]any{"operation": "get", "block_key": "k"})
	require.NoError(t, err)
	assert.Contains(t, string(out), "tracks task state")
	assert.Contains(t, string(out), `"content":"v"`)

	_, err = callCoreMemoryEdit(t, fn, ctx, map[string]any{"operation": "describe", "block_key": "missing", "description": "x"})
	require.Error(t, err)
	assert.Equal(t, apperr.CodeNotFound, apperr.CodeOf(err))
}

func keyFor(i int) string {
	return "block_" + string(rune('a'+i))
}
