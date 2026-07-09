package store_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/polarisagi/polaris/internal/memory/store"
	"github.com/polarisagi/polaris/pkg/types"

	_ "modernc.org/sqlite"
)

func TestSQLCoreMemoryStore(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	defer db.Close()

	ctx := context.Background()
	// core memory 属于 memory_schemas 之一，但可能 034 还未被 ApplyMemorySchemas 包含
	// 为了确保，手动执行 034
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS core_memory_blocks (
			agent_id TEXT NOT NULL,
			session_id TEXT NOT NULL,
			block_key TEXT NOT NULL,
			content TEXT NOT NULL,
			taint_level INTEGER NOT NULL,
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (agent_id, session_id, block_key)
		);
	`)
	require.NoError(t, err)

	coreMem := store.NewSQLCoreMemoryStore(db)

	agentID := "agent_1"
	sessionID := "sess_1"

	t.Run("Set and Get", func(t *testing.T) {
		err := coreMem.Set(ctx, agentID, sessionID, "persona", "I am a helpful assistant.", types.TaintNone)
		assert.NoError(t, err)

		block, err := coreMem.Get(ctx, agentID, sessionID, "persona")
		assert.NoError(t, err)
		assert.NotNil(t, block)
		assert.Equal(t, "I am a helpful assistant.", block.Content)
		assert.Equal(t, types.TaintNone, block.TaintLevel)
		assert.Equal(t, "persona", block.BlockKey)
		assert.WithinDuration(t, time.Now(), block.UpdatedAt, 2*time.Second)

		// Not found
		block2, err := coreMem.Get(ctx, agentID, sessionID, "unknown")
		assert.NoError(t, err)
		assert.Nil(t, block2)
	})

	t.Run("Set updates existing", func(t *testing.T) {
		err := coreMem.Set(ctx, agentID, sessionID, "persona", "I am a very helpful assistant.", types.TaintLow)
		assert.NoError(t, err)

		block, err := coreMem.Get(ctx, agentID, sessionID, "persona")
		assert.NoError(t, err)
		assert.Equal(t, "I am a very helpful assistant.", block.Content)
		assert.Equal(t, types.TaintLow, block.TaintLevel)
	})

	t.Run("List", func(t *testing.T) {
		err := coreMem.Set(ctx, agentID, sessionID, "task_state", "Step 1 done", types.TaintMedium)
		assert.NoError(t, err)

		blocks, err := coreMem.List(ctx, agentID, sessionID)
		assert.NoError(t, err)
		assert.Len(t, blocks, 2)
		assert.Equal(t, "persona", blocks[0].BlockKey)
		assert.Equal(t, "task_state", blocks[1].BlockKey)
	})

	t.Run("Delete", func(t *testing.T) {
		err := coreMem.Delete(ctx, agentID, sessionID, "persona")
		assert.NoError(t, err)

		block, err := coreMem.Get(ctx, agentID, sessionID, "persona")
		assert.NoError(t, err)
		assert.Nil(t, block)

		blocks, err := coreMem.List(ctx, agentID, sessionID)
		assert.NoError(t, err)
		assert.Len(t, blocks, 1)
		assert.Equal(t, "task_state", blocks[0].BlockKey)
	})
}
