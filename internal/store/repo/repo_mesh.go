package repo

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/protocol/repo"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// SQLiteMeshRepository 跨节点委派三张表（048_mesh.sql）的唯一 SQL 实现，供 internal/gateway/mesh 消费
// （接口在消费方定义，见 mesh.Repository）。
type SQLiteMeshRepository struct {
	db protocol.SQLQuerier
}

func NewSQLiteMeshRepository(db protocol.SQLQuerier) *SQLiteMeshRepository {
	return &SQLiteMeshRepository{db: db}
}

// ─── mesh_peer_tokens ───────────────────────────────────────────────────

func (r *SQLiteMeshRepository) InsertMeshToken(ctx context.Context, row repo.MeshTokenRow, tokenHash string) error {
	_, err := r.db.ExecContext(ctx, `INSERT INTO mesh_peer_tokens
		(token_id, token_hash, peer_name, peer_node_id, scopes, created_at, expires_at) VALUES (?,?,?,?,?,?,?)`,
		row.TokenID, tokenHash, row.PeerName, row.PeerNodeID, row.Scopes, row.CreatedAtMs, row.ExpiresAtMs)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteMeshRepository.InsertMeshToken", err)
	}
	return nil
}

// FindMeshTokenByHash 按摘要查找；不存在返回 nil, nil。
func (r *SQLiteMeshRepository) FindMeshTokenByHash(ctx context.Context, tokenHash string) (*repo.MeshTokenRow, error) {
	var t repo.MeshTokenRow
	err := r.db.QueryRowContext(ctx, `SELECT token_id, peer_name, peer_node_id, scopes, created_at, expires_at, revoked_at, last_used_at
		FROM mesh_peer_tokens WHERE token_hash = ?`, tokenHash).
		Scan(&t.TokenID, &t.PeerName, &t.PeerNodeID, &t.Scopes, &t.CreatedAtMs, &t.ExpiresAtMs, &t.RevokedAtMs, &t.LastUsedAtMs)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteMeshRepository.FindMeshTokenByHash", err)
	}
	return &t, nil
}

func (r *SQLiteMeshRepository) TouchMeshToken(ctx context.Context, tokenID string, usedAtMs int64) error {
	if _, err := r.db.ExecContext(ctx, `UPDATE mesh_peer_tokens SET last_used_at=? WHERE token_id=?`, usedAtMs, tokenID); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteMeshRepository.TouchMeshToken", err)
	}
	return nil
}

// RevokeMeshToken 幂等吊销；token_id 不存在返回 NotFound。
func (r *SQLiteMeshRepository) RevokeMeshToken(ctx context.Context, tokenID string, revokedAtMs int64) error {
	if _, err := r.db.ExecContext(ctx, `UPDATE mesh_peer_tokens SET revoked_at=? WHERE token_id=? AND revoked_at=0`,
		revokedAtMs, tokenID); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteMeshRepository.RevokeMeshToken", err)
	}
	var one int
	if err := r.db.QueryRowContext(ctx, `SELECT 1 FROM mesh_peer_tokens WHERE token_id=?`, tokenID).Scan(&one); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return apperr.New(apperr.CodeNotFound, "mesh: no such peer token "+tokenID)
		}
		return apperr.Wrap(apperr.CodeInternal, "SQLiteMeshRepository.RevokeMeshToken", err)
	}
	return nil
}

func (r *SQLiteMeshRepository) ListMeshTokens(ctx context.Context) ([]repo.MeshTokenRow, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT token_id, peer_name, peer_node_id, scopes, created_at, expires_at, revoked_at, last_used_at
		FROM mesh_peer_tokens ORDER BY created_at DESC`)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteMeshRepository.ListMeshTokens", err)
	}
	defer rows.Close()
	var out []repo.MeshTokenRow
	for rows.Next() {
		var t repo.MeshTokenRow
		if err := rows.Scan(&t.TokenID, &t.PeerName, &t.PeerNodeID, &t.Scopes, &t.CreatedAtMs, &t.ExpiresAtMs, &t.RevokedAtMs, &t.LastUsedAtMs); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteMeshRepository.ListMeshTokens scan", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteMeshRepository.ListMeshTokens iterate", err)
	}
	return out, nil
}

// RebindMeshToken 把 peer_name 下、仍绑定在 fromNodeID 的最新有效令牌改绑到 toNodeID；返回被改绑的 token_id（无则空串）。
func (r *SQLiteMeshRepository) RebindMeshToken(ctx context.Context, peerName, fromNodeID, toNodeID string) (string, error) {
	var tokenID string
	err := r.db.QueryRowContext(ctx, `SELECT token_id FROM mesh_peer_tokens
		WHERE peer_name=? AND peer_node_id=? AND revoked_at=0 ORDER BY created_at DESC LIMIT 1`, peerName, fromNodeID).Scan(&tokenID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "SQLiteMeshRepository.RebindMeshToken", err)
	}
	if _, err := r.db.ExecContext(ctx, `UPDATE mesh_peer_tokens SET peer_node_id=? WHERE token_id=?`, toNodeID, tokenID); err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "SQLiteMeshRepository.RebindMeshToken", err)
	}
	return tokenID, nil
}

// ─── mesh_peer_tasks ────────────────────────────────────────────────────

func (r *SQLiteMeshRepository) InsertMeshTask(ctx context.Context, t repo.MeshTaskRow) error {
	_, err := r.db.ExecContext(ctx, `INSERT INTO mesh_peer_tasks
		(task_id, bb_task_id, caller_node_id, caller_name, token_id, protocol, context_id, message_id, target_agent,
		 spawn_depth, chain, input_text, state, status_message, created_at, updated_at, expires_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		t.TaskID, t.BBTaskID, t.CallerNodeID, t.CallerName, t.TokenID, t.Protocol, t.ContextID, t.MessageID, t.TargetAgent,
		t.SpawnDepth, t.Chain, t.InputText, t.State, t.StatusMessage, t.CreatedAtMs, t.UpdatedAtMs, t.ExpiresAtMs)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteMeshRepository.InsertMeshTask", err)
	}
	return nil
}

func (r *SQLiteMeshRepository) UpdateMeshTaskState(ctx context.Context, taskID, state, statusMessage, result string, updatedAtMs int64) error {
	if _, err := r.db.ExecContext(ctx, `UPDATE mesh_peer_tasks SET state=?, status_message=?, result=?, updated_at=? WHERE task_id=?`,
		state, statusMessage, result, updatedAtMs, taskID); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteMeshRepository.UpdateMeshTaskState", err)
	}
	return nil
}

func (r *SQLiteMeshRepository) MarkMeshTaskCanceled(ctx context.Context, taskID string) error {
	if _, err := r.db.ExecContext(ctx, `UPDATE mesh_peer_tasks SET canceled=1 WHERE task_id=?`, taskID); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteMeshRepository.MarkMeshTaskCanceled", err)
	}
	return nil
}

func (r *SQLiteMeshRepository) DeleteExpiredMeshTasks(ctx context.Context, nowMs int64) error {
	if _, err := r.db.ExecContext(ctx, `DELETE FROM mesh_peer_tasks WHERE expires_at < ?`, nowMs); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteMeshRepository.DeleteExpiredMeshTasks", err)
	}
	return nil
}

func meshTaskWhere(f repo.MeshTaskFilter, paged bool) (string, []any) {
	var where []string
	var args []any
	add := func(cond string, v any) { where, args = append(where, cond), append(args, v) }
	if f.TaskID != "" {
		add("task_id = ?", f.TaskID)
	}
	if f.CallerNodeID != "" {
		add("caller_node_id = ?", f.CallerNodeID)
	}
	if f.MessageID != "" {
		add("message_id = ?", f.MessageID)
	}
	if f.ContextID != "" {
		add("context_id = ?", f.ContextID)
	}
	if f.State != "" {
		add("state = ?", f.State)
	}
	if f.ActiveOnly {
		where = append(where, "state IN ('submitted','working','input_required')")
	}
	if paged && f.BeforeTaskID != "" {
		where = append(where, "(created_at < ? OR (created_at = ? AND task_id < ?))")
		args = append(args, f.BeforeCreatedMs, f.BeforeCreatedMs, f.BeforeTaskID)
	}
	if len(where) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(where, " AND "), args
}

// CountMeshTasks 按过滤条件计数（忽略分页游标）。
func (r *SQLiteMeshRepository) CountMeshTasks(ctx context.Context, f repo.MeshTaskFilter) (int, error) {
	where, args := meshTaskWhere(f, false)
	var n int
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM mesh_peer_tasks`+where, args...).Scan(&n); err != nil {
		return 0, apperr.Wrap(apperr.CodeInternal, "SQLiteMeshRepository.CountMeshTasks", err)
	}
	return n, nil
}

// ListMeshTasks 按过滤条件列出，默认 (created_at, task_id) 倒序。
func (r *SQLiteMeshRepository) ListMeshTasks(ctx context.Context, f repo.MeshTaskFilter) ([]repo.MeshTaskRow, error) {
	where, args := meshTaskWhere(f, true)
	order := " ORDER BY created_at DESC, task_id DESC"
	if f.OldestFirst {
		order = " ORDER BY created_at ASC, task_id ASC"
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	args = append(args, limit)
	rows, err := r.db.QueryContext(ctx, `SELECT task_id, bb_task_id, caller_node_id, caller_name, token_id, protocol, context_id,
		message_id, target_agent, spawn_depth, chain, input_text, state, status_message, result, canceled,
		created_at, updated_at, expires_at FROM mesh_peer_tasks`+where+order+` LIMIT ?`, args...)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteMeshRepository.ListMeshTasks", err)
	}
	defer rows.Close()
	var out []repo.MeshTaskRow
	for rows.Next() {
		var t repo.MeshTaskRow
		var canceled int
		if err := rows.Scan(&t.TaskID, &t.BBTaskID, &t.CallerNodeID, &t.CallerName, &t.TokenID, &t.Protocol, &t.ContextID,
			&t.MessageID, &t.TargetAgent, &t.SpawnDepth, &t.Chain, &t.InputText, &t.State, &t.StatusMessage, &t.Result,
			&canceled, &t.CreatedAtMs, &t.UpdatedAtMs, &t.ExpiresAtMs); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteMeshRepository.ListMeshTasks scan", err)
		}
		t.Canceled = canceled != 0
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteMeshRepository.ListMeshTasks iterate", err)
	}
	return out, nil
}

// ─── mesh_peers ─────────────────────────────────────────────────────────

// UpsertMeshPeer 按 name 登记或更新；node_id 已被其他名字占用时返回 AlreadyExists。
func (r *SQLiteMeshRepository) UpsertMeshPeer(ctx context.Context, p repo.MeshPeerRow) error {
	_, err := r.db.ExecContext(ctx, `INSERT INTO mesh_peers (name, node_id, url, public_key, issued_token_id, created_at)
		VALUES (?,?,?,?,?,?)
		ON CONFLICT(name) DO UPDATE SET node_id=excluded.node_id, url=excluded.url, public_key=excluded.public_key,
			issued_token_id=CASE WHEN excluded.issued_token_id != '' THEN excluded.issued_token_id ELSE mesh_peers.issued_token_id END`,
		p.Name, p.NodeID, p.URL, p.PublicKey, p.IssuedTokenID, p.CreatedAtMs)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return apperr.New(apperr.CodeAlreadyExists, "mesh: node "+p.NodeID+" is already registered under another name")
		}
		return apperr.Wrap(apperr.CodeInternal, "SQLiteMeshRepository.UpsertMeshPeer", err)
	}
	return nil
}

// DeleteMeshPeer 删除登记；不存在返回 NotFound。
func (r *SQLiteMeshRepository) DeleteMeshPeer(ctx context.Context, name string) error {
	res, err := r.db.ExecContext(ctx, `DELETE FROM mesh_peers WHERE name=?`, name)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteMeshRepository.DeleteMeshPeer", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return apperr.New(apperr.CodeNotFound, "mesh: no such peer "+name)
	}
	return nil
}

func (r *SQLiteMeshRepository) UpdateMeshPeerSeen(ctx context.Context, name string, seenAtMs int64, errMsg string) error {
	var err error
	if errMsg == "" {
		_, err = r.db.ExecContext(ctx, `UPDATE mesh_peers SET last_seen_at=?, last_error='' WHERE name=?`, seenAtMs, name)
	} else {
		_, err = r.db.ExecContext(ctx, `UPDATE mesh_peers SET last_error=? WHERE name=?`, errMsg, name)
	}
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteMeshRepository.UpdateMeshPeerSeen", err)
	}
	return nil
}

// ListMeshPeers 全部对端（name 为空）或指定一个（name 非空），按 name 排序。
func (r *SQLiteMeshRepository) ListMeshPeers(ctx context.Context, name string) ([]repo.MeshPeerRow, error) {
	q := `SELECT name, node_id, url, public_key, issued_token_id, created_at, last_seen_at, last_error FROM mesh_peers`
	var args []any
	if name != "" {
		q += ` WHERE name=?`
		args = append(args, name)
	}
	rows, err := r.db.QueryContext(ctx, q+` ORDER BY name`, args...)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteMeshRepository.ListMeshPeers", err)
	}
	defer rows.Close()
	var out []repo.MeshPeerRow
	for rows.Next() {
		var p repo.MeshPeerRow
		if err := rows.Scan(&p.Name, &p.NodeID, &p.URL, &p.PublicKey, &p.IssuedTokenID, &p.CreatedAtMs, &p.LastSeenAtMs, &p.LastError); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteMeshRepository.ListMeshPeers scan", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteMeshRepository.ListMeshPeers iterate", err)
	}
	return out, nil
}
