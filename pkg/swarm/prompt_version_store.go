package swarm

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	perrors "github.com/polarisagi/polaris/internal/errors"
)

// PromptVersionStore prompt_versions 表的读写层。
// DDL 权威源：internal/protocol/schema/010_self_improve.sql
// 写路径遵循 XR-04：非实时写用 MutationBus，此处用直接写（CAS Activate 需事务保证原子性）。
type PromptVersionStore struct {
	db *sql.DB
	// OnActivate 版本激活成功后的回调，可选。
	// 调用方（如 M13 Interface 层）注入此函数以接收激活通知，用于热更新 ImmutableCore。
	// nil 时跳过回调，不影响激活逻辑。
	OnActivate func(taskType, promptText string)
}

// NewPromptVersionStore 创建版本存储，db 必须非 nil。
func NewPromptVersionStore(db *sql.DB) *PromptVersionStore {
	return &PromptVersionStore{db: db}
}

// Save 写入候选版本（is_active=0）。ID 已存在则忽略（幂等）。
func (s *PromptVersionStore) Save(ctx context.Context, v *PromptVersion) error {
	if v.ID == "" {
		return perrors.New(perrors.CodeInvalidInput, "prompt_version_store: version ID must not be empty")
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO prompt_versions
			(id, version, task_type, prompt_text, score, cost, source, parent_version, is_active, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, ?)
		ON CONFLICT(id) DO NOTHING
	`, v.ID, v.Version, v.TaskType, v.Prompt, v.Score, v.Cost, v.Source, v.ParentVer,
		time.Now().Unix())
	if err != nil {
		return perrors.Wrap(perrors.CodeInternal, "prompt_version_store: save failed", err)
	}
	return nil
}

// GetActive 获取当前激活版本；无激活版本时返回 nil, nil。
func (s *PromptVersionStore) GetActive(ctx context.Context, taskType string) (*PromptVersion, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, version, task_type, prompt_text, score, cost, source, parent_version
		FROM prompt_versions
		WHERE task_type = ? AND is_active = 1
		ORDER BY created_at DESC LIMIT 1
	`, taskType)
	v := &PromptVersion{}
	err := row.Scan(&v.ID, &v.Version, &v.TaskType, &v.Prompt, &v.Score, &v.Cost, &v.Source, &v.ParentVer)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, "prompt_version_store: get active failed", err)
	}
	v.Active = true
	return v, nil
}

// UpdateScore 更新候选版本的 Eval 评分。
func (s *PromptVersionStore) UpdateScore(ctx context.Context, id string, score float64) error {
	res, err := s.db.ExecContext(ctx,
		"UPDATE prompt_versions SET score = ? WHERE id = ?", score, id)
	if err != nil {
		return perrors.Wrap(perrors.CodeInternal, "prompt_version_store: update score failed", err)
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return perrors.New(perrors.CodeNotFound, fmt.Sprintf("prompt_version_store: version %q not found", id))
	}
	return nil
}

// Activate 将指定版本设为激活（原子 CAS：置旧 active=0 → 置新 active=1）。
// 若 score < baselineScore，返回错误，不激活。
func (s *PromptVersionStore) Activate(ctx context.Context, taskType, id string, baselineScore float64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return perrors.Wrap(perrors.CodeInternal, "prompt_version_store: begin tx failed", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 读候选版本评分
	var candidateScore float64
	err = tx.QueryRowContext(ctx,
		"SELECT score FROM prompt_versions WHERE id = ? AND task_type = ?", id, taskType).
		Scan(&candidateScore)
	if errors.Is(err, sql.ErrNoRows) {
		return perrors.New(perrors.CodeNotFound, fmt.Sprintf("prompt_version_store: candidate %q not found", id))
	}
	if err != nil {
		return perrors.Wrap(perrors.CodeInternal, "prompt_version_store: read candidate score failed", err)
	}
	if candidateScore < baselineScore {
		return perrors.New(perrors.CodeInvalidInput,
			fmt.Sprintf("prompt_version_store: candidate score %.3f < baseline %.3f, skip activate", candidateScore, baselineScore))
	}

	// 置旧激活版本为 inactive
	if _, err = tx.ExecContext(ctx,
		"UPDATE prompt_versions SET is_active = 0 WHERE task_type = ? AND is_active = 1", taskType); err != nil {
		return perrors.Wrap(perrors.CodeInternal, "prompt_version_store: deactivate old failed", err)
	}
	// 激活候选
	var promptText string
	if err = tx.QueryRowContext(ctx,
		"SELECT prompt_text FROM prompt_versions WHERE id = ?", id).Scan(&promptText); err != nil {
		return perrors.Wrap(perrors.CodeInternal, "prompt_version_store: read prompt text failed", err)
	}
	if _, err = tx.ExecContext(ctx,
		"UPDATE prompt_versions SET is_active = 1 WHERE id = ?", id); err != nil {
		return perrors.Wrap(perrors.CodeInternal, "prompt_version_store: activate failed", err)
	}
	if err = tx.Commit(); err != nil {
		return perrors.Wrap(perrors.CodeInternal, "prompt_version_store: commit failed", err)
	}
	// 激活成功后通知注入方（如 M13 Interface 层热更新 ImmutableCore）
	if s.OnActivate != nil && promptText != "" {
		s.OnActivate(taskType, promptText)
	}
	return nil
}

// ListRecent 获取最近 n 个版本（按 created_at DESC）；n≤0 时返回最近 10 条。
func (s *PromptVersionStore) ListRecent(ctx context.Context, taskType string, n int) ([]*PromptVersion, error) {
	if n <= 0 {
		n = 10
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, version, task_type, prompt_text, score, cost, source, parent_version, is_active
		FROM prompt_versions
		WHERE task_type = ?
		ORDER BY created_at DESC LIMIT ?
	`, taskType, n)
	if err != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, "prompt_version_store: list recent failed", err)
	}
	defer rows.Close()

	var result []*PromptVersion
	for rows.Next() {
		v := &PromptVersion{}
		var isActive int
		if err = rows.Scan(&v.ID, &v.Version, &v.TaskType, &v.Prompt,
			&v.Score, &v.Cost, &v.Source, &v.ParentVer, &isActive); err != nil {
			return nil, perrors.Wrap(perrors.CodeInternal, "prompt_version_store: scan failed", err)
		}
		v.Active = isActive == 1
		result = append(result, v)
	}
	return result, rows.Err()
}
