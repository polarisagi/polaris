package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// SQLCoreMemoryStore 实现了 protocol.CoreMemory 接口，持久化到 core_memory_blocks 表。
type SQLCoreMemoryStore struct {
	// 2026-08-08：原为 *sql.DB。inv_NoRawSQLDBField 要求 storage 层外持接口而非
	// 具体连接；本类型只用 Exec/Query/QueryRow，protocol.SQLQuerier 恰好覆盖。
	db protocol.SQLQuerier
}

func NewSQLCoreMemoryStore(db protocol.SQLQuerier) *SQLCoreMemoryStore {
	return &SQLCoreMemoryStore{db: db}
}

func scanCoreMemoryBlock(row interface {
	Scan(dest ...any) error
}) (*types.CoreMemoryBlock, error) {
	var block types.CoreMemoryBlock
	var taintLevel, readOnly int
	err := row.Scan(&block.AgentID, &block.SessionID, &block.BlockKey, &block.Content, &taintLevel,
		&block.UpdatedAt, &block.Description, &readOnly, &block.MaxBytes)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, err //nolint:wrapcheck // 保留 sql.ErrNoRows 哨兵身份，供调用方 errors.Is 判断
		}
		return nil, apperr.Wrap(apperr.CodeInternal, "core_memory_edit: scan row failed", err)
	}
	block.TaintLevel = types.TaintLevel(taintLevel)
	block.ReadOnly = readOnly != 0
	block.SizeBytes = len(block.Content)
	return &block, nil
}

const coreMemorySelectCols = `agent_id, session_id, block_key, content, taint_level, updated_at, description, read_only, max_bytes`

func (s *SQLCoreMemoryStore) Get(ctx context.Context, agentID, sessionID, blockKey string) (*types.CoreMemoryBlock, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+coreMemorySelectCols+`
		FROM core_memory_blocks
		WHERE agent_id = ? AND session_id = ? AND block_key = ?`,
		agentID, sessionID, blockKey,
	)

	block, err := scanCoreMemoryBlock(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil // Not found is not an error
	}
	if err != nil {
		return nil, err // scanCoreMemoryBlock 已用 apperr 包装，不重复包装
	}
	return block, nil
}

// defaultBlockMaxBytes 新建块的字节上限，固化自 state.yaml SSoT
// m5_memory.core_memory_block_max_kb（ADR-0082：不追溯已存在行）。
func defaultBlockMaxBytes() int {
	return config.DefaultThresholds().M5Memory.CoreMemoryBlockMaxKB * 1024
}

func (s *SQLCoreMemoryStore) Set(ctx context.Context, agentID, sessionID, blockKey, content string, taintLevel types.TaintLevel) error {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO core_memory_blocks (agent_id, session_id, block_key, content, taint_level, updated_at, max_bytes)
		VALUES (?, ?, ?, ?, ?, CURRENT_TIMESTAMP, ?)
		ON CONFLICT(agent_id, session_id, block_key) DO UPDATE SET
			content = excluded.content,
			taint_level = excluded.taint_level,
			updated_at = excluded.updated_at
		WHERE core_memory_blocks.read_only = 0`,
		agentID, sessionID, blockKey, content, int(taintLevel), defaultBlockMaxBytes(),
	)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "failed to set core memory block", err)
	}
	if res != nil {
		rows, rowsErr := res.RowsAffected()
		if rowsErr == nil && rows == 0 {
			existing, getErr := s.Get(ctx, agentID, sessionID, blockKey)
			if getErr == nil && existing != nil && existing.ReadOnly {
				return apperr.New(apperr.CodeForbidden, "core_memory_set: block is read-only")
			}
		}
	}
	return nil
}

func (s *SQLCoreMemoryStore) Delete(ctx context.Context, agentID, sessionID, blockKey string) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM core_memory_blocks WHERE agent_id = ? AND session_id = ? AND block_key = ? AND read_only = 0`,
		agentID, sessionID, blockKey,
	)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "failed to delete core memory block", err)
	}
	if res != nil {
		rows, rowsErr := res.RowsAffected()
		if rowsErr == nil && rows == 0 {
			existing, getErr := s.Get(ctx, agentID, sessionID, blockKey)
			if getErr == nil && existing != nil && existing.ReadOnly {
				return apperr.New(apperr.CodeForbidden, "core_memory_delete: block is read-only")
			}
		}
	}
	return nil
}

func (s *SQLCoreMemoryStore) List(ctx context.Context, agentID, sessionID string) ([]types.CoreMemoryBlock, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+coreMemorySelectCols+`
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
		block, err := scanCoreMemoryBlock(rows)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "failed to scan core memory block", err)
		}
		blocks = append(blocks, *block)
	}
	if err := rows.Err(); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "failed to iterate core memory blocks", err)
	}

	return blocks, nil
}

// Replace 在块内做精确子串替换（MemFS 语义，ADR-0082）。read_only/max_bytes 策略
// 在此就地校验——Replace 是唯一同时持有"旧内容"与"新内容"的位置，策略检查放在
// 调用方（工具 exec 层）需要重复计算子串替换结果，故内聚于此。
func (s *SQLCoreMemoryStore) Replace(ctx context.Context, agentID, sessionID, blockKey, old, newStr string, replaceAll bool, taintLevel types.TaintLevel) (int, error) {
	block, err := s.Get(ctx, agentID, sessionID, blockKey)
	if err != nil {
		return 0, err
	}
	if block == nil {
		return 0, apperr.New(apperr.CodeNotFound, "core_memory_edit: block not found")
	}
	if block.ReadOnly {
		return 0, apperr.New(apperr.CodeForbidden, "core_memory_edit: block is read-only")
	}

	occurrences := strings.Count(block.Content, old)
	if occurrences == 0 {
		return 0, apperr.New(apperr.CodeNotFound, "core_memory_edit: old_str not found in block")
	}
	if occurrences > 1 && !replaceAll {
		return occurrences, apperr.New(apperr.CodeInvalidInput,
			"core_memory_edit: old_str matches multiple times, provide more unique context or set replace_all=true")
	}

	var newContent string
	if replaceAll {
		newContent = strings.ReplaceAll(block.Content, old, newStr)
	} else {
		newContent = strings.Replace(block.Content, old, newStr, 1)
	}

	maxBytes := block.MaxBytes
	if maxBytes <= 0 {
		maxBytes = defaultBlockMaxBytes()
	}
	if len(newContent) > maxBytes {
		return occurrences, apperr.New(apperr.CodeInvalidInput,
			"core_memory_edit: replace result exceeds block byte limit")
	}

	effectiveTaint := block.TaintLevel
	if taintLevel > effectiveTaint {
		effectiveTaint = taintLevel // 只升不降
	}

	_, err = s.db.ExecContext(ctx,
		`UPDATE core_memory_blocks SET content = ?, taint_level = ?, updated_at = CURRENT_TIMESTAMP
		WHERE agent_id = ? AND session_id = ? AND block_key = ?`,
		newContent, int(effectiveTaint), agentID, sessionID, blockKey,
	)
	if err != nil {
		return occurrences, apperr.Wrap(apperr.CodeInternal, "core_memory_edit: replace write failed", err)
	}
	return occurrences, nil
}

// Describe 设置块的用途说明。允许作用于 read_only 块（元信息，非受保护内容本身）。
func (s *SQLCoreMemoryStore) Describe(ctx context.Context, agentID, sessionID, blockKey, description string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE core_memory_blocks SET description = ? WHERE agent_id = ? AND session_id = ? AND block_key = ?`,
		description, agentID, sessionID, blockKey,
	)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "core_memory_edit: describe failed", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "core_memory_edit: describe rows-affected failed", err)
	}
	if n == 0 {
		return apperr.New(apperr.CodeNotFound, "core_memory_edit: block not found")
	}
	return nil
}
