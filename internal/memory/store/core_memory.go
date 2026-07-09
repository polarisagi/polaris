package store

import (
	"context"
	"database/sql"

	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// SQLCoreMemoryStore 实现了 protocol.CoreMemory 接口，持久化到 core_memory_blocks 表。
type SQLCoreMemoryStore struct {
	db *sql.DB
}

func NewSQLCoreMemoryStore(db *sql.DB) *SQLCoreMemoryStore {
	return &SQLCoreMemoryStore{db: db}
}

func (s *SQLCoreMemoryStore) Get(ctx context.Context, agentID, sessionID, blockKey string) (*types.CoreMemoryBlock, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT agent_id, session_id, block_key, content, taint_level, updated_at
		FROM core_memory_blocks
		WHERE agent_id = ? AND session_id = ? AND block_key = ?`,
		agentID, sessionID, blockKey,
	)

	var block types.CoreMemoryBlock
	var taintLevel int
	err := row.Scan(&block.AgentID, &block.SessionID, &block.BlockKey, &block.Content, &taintLevel, &block.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil // Not found is not an error
	}
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "failed to get core memory block", err)
	}
	block.TaintLevel = types.TaintLevel(taintLevel)
	return &block, nil
}

func (s *SQLCoreMemoryStore) Set(ctx context.Context, agentID, sessionID, blockKey, content string, taintLevel types.TaintLevel) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO core_memory_blocks (agent_id, session_id, block_key, content, taint_level, updated_at)
		VALUES (?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(agent_id, session_id, block_key) DO UPDATE SET
			content = excluded.content,
			taint_level = excluded.taint_level,
			updated_at = excluded.updated_at`,
		agentID, sessionID, blockKey, content, int(taintLevel),
	)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "failed to set core memory block", err)
	}
	return nil
}

func (s *SQLCoreMemoryStore) Delete(ctx context.Context, agentID, sessionID, blockKey string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM core_memory_blocks WHERE agent_id = ? AND session_id = ? AND block_key = ?`,
		agentID, sessionID, blockKey,
	)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "failed to delete core memory block", err)
	}
	return nil
}

func (s *SQLCoreMemoryStore) List(ctx context.Context, agentID, sessionID string) ([]types.CoreMemoryBlock, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT agent_id, session_id, block_key, content, taint_level, updated_at
		FROM core_memory_blocks
		WHERE agent_id = ? AND session_id = ?
		ORDER BY block_key ASC`,
		agentID, sessionID,
	)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "failed to list core memory blocks", err)
	}
	defer rows.Close()

	var blocks []types.CoreMemoryBlock
	for rows.Next() {
		var block types.CoreMemoryBlock
		var taintLevel int
		if err := rows.Scan(&block.AgentID, &block.SessionID, &block.BlockKey, &block.Content, &taintLevel, &block.UpdatedAt); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "failed to scan core memory block", err)
		}
		block.TaintLevel = types.TaintLevel(taintLevel)
		blocks = append(blocks, block)
	}
	if err := rows.Err(); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "failed to iterate core memory blocks", err)
	}

	return blocks, nil
}
