package repo

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/protocol/repo"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// SQLiteTrajectoryRepository session_trajectory（045）的读写实现。
// 写侧必须传写连接（单写者），读侧可传读连接；两者语义相同，由装配点决定。
type SQLiteTrajectoryRepository struct {
	db protocol.SQLQuerier
}

func NewSQLiteTrajectoryRepository(db protocol.SQLQuerier) *SQLiteTrajectoryRepository {
	return &SQLiteTrajectoryRepository{db: db}
}

const trajectoryCols = `id, session_id, seq, event_type, tool_name, tool_ok, latency_ms, payload, created_at`

// appendSeqRetries seq 分配撞 UNIQUE 时的重试次数。单写连接下不会发生；
// 仅在测试或误传多连接池时兜底，避免一次冲突就丢一条轨迹。
const appendSeqRetries = 3

// Append 追加一行并返回分配到的 seq。seq 用单语句 INSERT…SELECT COALESCE(MAX)+1 分配，
// 不在应用层读后写，避免并发写入同一会话时算出重复 seq。
// toolName 为空 → NULL；toolOK 为 nil → NULL；latencyMs < 0 → NULL。
func (r *SQLiteTrajectoryRepository) Append(
	ctx context.Context, sessionID, eventType, toolName string, toolOK *bool, latencyMs int64, payload string,
) (int64, error) {
	var tn, ok, lat any
	if toolName != "" {
		tn = toolName
	}
	if toolOK != nil {
		if *toolOK {
			ok = 1
		} else {
			ok = 0
		}
	}
	if latencyMs >= 0 {
		lat = latencyMs
	}
	var lastErr error
	for range appendSeqRetries {
		var seq int64
		err := r.db.QueryRowContext(ctx,
			`INSERT INTO session_trajectory(session_id, seq, event_type, tool_name, tool_ok, latency_ms, payload, created_at)
			 SELECT ?, COALESCE(MAX(seq),0)+1, ?, ?, ?, ?, ?, ? FROM session_trajectory WHERE session_id=?
			 RETURNING seq`,
			sessionID, eventType, tn, ok, lat, payload, time.Now().UnixMilli(), sessionID).Scan(&seq)
		if err == nil {
			return seq, nil
		}
		lastErr = err
		if !strings.Contains(err.Error(), "UNIQUE") {
			break
		}
	}
	return 0, apperr.Wrap(apperr.CodeInternal, "SQLiteTrajectoryRepository.Append", lastErr)
}

// ListBySession 按 seq 升序返回会话全部事件。
func (r *SQLiteTrajectoryRepository) ListBySession(ctx context.Context, sessionID string) ([]repo.TrajectoryRow, error) {
	return r.query(ctx, "ListBySession",
		`SELECT `+trajectoryCols+` FROM session_trajectory WHERE session_id=? ORDER BY seq`, sessionID)
}

// ListToolBySession 按 seq 升序返回会话内的工具事件（任务画布用）。
func (r *SQLiteTrajectoryRepository) ListToolBySession(ctx context.Context, sessionID string) ([]repo.TrajectoryRow, error) {
	return r.query(ctx, "ListToolBySession",
		`SELECT `+trajectoryCols+` FROM session_trajectory
		 WHERE session_id=? AND tool_name IS NOT NULL ORDER BY seq`, sessionID)
}

// RecentToolRows 每个工具取最近 perTool 条工具事件，按 id 升序（旧→新）返回，
// 供 PolicyEvolver 按时间序重放进滑动窗口。WHERE tool_name IS NOT NULL 使其命中部分索引。
func (r *SQLiteTrajectoryRepository) RecentToolRows(ctx context.Context, perTool int) ([]repo.TrajectoryRow, error) {
	if perTool <= 0 {
		return nil, nil
	}
	return r.query(ctx, "RecentToolRows",
		`SELECT `+trajectoryCols+` FROM (
		   SELECT `+trajectoryCols+`, ROW_NUMBER() OVER (PARTITION BY tool_name ORDER BY id DESC) AS rn
		   FROM session_trajectory WHERE tool_name IS NOT NULL
		 ) WHERE rn <= ? ORDER BY id`, perTool)
}

// RecentToolSequences 取最近有工具事件的 maxSessions 个会话（且 created_at >= sinceMs），
// 每个会话一条按 seq 排序的工具名序列，供 Markov 矩阵 warm-start。会话间按最近活跃先后升序。
func (r *SQLiteTrajectoryRepository) RecentToolSequences(ctx context.Context, maxSessions int, sinceMs int64) ([][]string, error) {
	if maxSessions <= 0 {
		return nil, nil
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT session_id, tool_name FROM session_trajectory
		 WHERE tool_name IS NOT NULL AND session_id IN (
		   SELECT session_id FROM session_trajectory
		   WHERE tool_name IS NOT NULL AND created_at >= ?
		   GROUP BY session_id ORDER BY MAX(id) DESC LIMIT ?)
		 ORDER BY session_id, seq`, sinceMs, maxSessions)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteTrajectoryRepository.RecentToolSequences", err)
	}
	defer rows.Close()
	var out [][]string
	var cur string
	for rows.Next() {
		var sid, tool string
		if err := rows.Scan(&sid, &tool); err != nil {
			return out, apperr.Wrap(apperr.CodeInternal, "SQLiteTrajectoryRepository.RecentToolSequences scan", err)
		}
		if len(out) == 0 || sid != cur {
			out = append(out, nil)
			cur = sid
		}
		out[len(out)-1] = append(out[len(out)-1], tool)
	}
	if err := rows.Err(); err != nil {
		return out, apperr.Wrap(apperr.CodeInternal, "SQLiteTrajectoryRepository.RecentToolSequences iterate", err)
	}
	return out, nil
}

func (r *SQLiteTrajectoryRepository) query(ctx context.Context, op, q string, args ...any) ([]repo.TrajectoryRow, error) {
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteTrajectoryRepository."+op, err)
	}
	defer rows.Close()
	var out []repo.TrajectoryRow
	for rows.Next() {
		var (
			row repo.TrajectoryRow
			tn  sql.NullString
			ok  sql.NullInt64
			lat sql.NullInt64
		)
		if err := rows.Scan(&row.ID, &row.SessionID, &row.Seq, &row.EventType, &tn, &ok, &lat, &row.Payload, &row.CreatedAtMs); err != nil {
			return out, apperr.Wrap(apperr.CodeInternal, "SQLiteTrajectoryRepository."+op+" scan", err)
		}
		row.ToolName = tn.String
		if ok.Valid {
			b := ok.Int64 != 0
			row.ToolOK = &b
		}
		row.LatencyMs = -1
		if lat.Valid {
			row.LatencyMs = lat.Int64
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return out, apperr.Wrap(apperr.CodeInternal, "SQLiteTrajectoryRepository."+op+" iterate", err)
	}
	return out, nil
}
