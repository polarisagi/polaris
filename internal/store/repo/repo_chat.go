package repo

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"

	"github.com/polarisagi/polaris/internal/protocol"
	protorepo "github.com/polarisagi/polaris/internal/protocol/repo"
	"github.com/polarisagi/polaris/pkg/types"
	"github.com/polarisagi/polaris/pkg/util"
)

// SQLiteChatRepository 实现 protocol.ChatRepository。
// 操作 chat_sessions, chat_messages 表以及 messages_fts 虚拟表。
// @arch: docs/arch/M02-Storage-Fabric.md
type SQLiteChatRepository struct {
	db *sql.DB
}

var _ protocol.ChatRepository = (*SQLiteChatRepository)(nil)

func NewSQLiteChatRepository(db *sql.DB) *SQLiteChatRepository {
	return &SQLiteChatRepository{db: db}
}

// CreateSession 创建一个新会话
func (r *SQLiteChatRepository) CreateSession(ctx context.Context, row types.ChatSessionRow) error {
	// project_id 空串按默认项目处理；INSERT OR IGNORE 保证已存在会话不被改归属
	// （聊天请求带不同 project_id 时以库内为准，ADR-0097 决策一）。
	projectID := row.ProjectID
	if projectID == "" {
		projectID = protorepo.DefaultProjectID
	}
	// INSERT ... SELECT 以项目存在且未归档为前提：归档项目不再接收新会话，
	// 但其中已有会话照常可用（它们走 INSERT OR IGNORE 的"已存在"分支）。
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := r.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO chat_sessions(id, title, project_id, thrashing_index, created_at, updated_at)
		 SELECT ?, ?, p.id, ?, ?, ? FROM projects p WHERE p.id = ? AND p.archived = 0`,
		row.ID, row.Title, row.ThrashingIndex, now, now, projectID)
	if err != nil {
		if strings.Contains(err.Error(), "FOREIGN KEY") {
			return apperr.Wrap(apperr.CodeNotFound, "project not found", err)
		}
		return apperr.Wrap(apperr.CodeInternal, "SQLiteChatRepository.CreateSession", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	return r.explainNoInsert(ctx, row.ID, projectID)
}

// explainNoInsert 区分 CreateSession 未插入的三种原因：会话已存在（正常）、
// 项目不存在、项目已归档。
func (r *SQLiteChatRepository) explainNoInsert(ctx context.Context, sessionID, projectID string) error {
	var one int
	err := r.db.QueryRowContext(ctx, `SELECT 1 FROM chat_sessions WHERE id=?`, sessionID).Scan(&one)
	if err == nil {
		return nil
	}
	if err != sql.ErrNoRows {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteChatRepository.CreateSession check session", err)
	}
	var archived int
	err = r.db.QueryRowContext(ctx, `SELECT archived FROM projects WHERE id=?`, projectID).Scan(&archived)
	if err == sql.ErrNoRows {
		return apperr.Wrap(apperr.CodeNotFound, "project not found", err)
	}
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteChatRepository.CreateSession check project", err)
	}
	return apperr.New(apperr.CodeInvalidInput, "项目已归档，不能在其中新建会话")
}

// GetSession 获取会话信息
func (r *SQLiteChatRepository) GetSession(ctx context.Context, id string) (*types.ChatSessionRow, error) {
	var row types.ChatSessionRow
	err := r.db.QueryRowContext(ctx,
		`SELECT id, title, project_id, thrashing_index, created_at, updated_at FROM chat_sessions WHERE id=?`, id,
	).Scan(&row.ID, &row.Title, &row.ProjectID, &row.ThrashingIndex, &row.CreatedAt, &row.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteChatRepository.GetSession", err)
	}
	return &row, nil
}

// ListSessions 列出最近的会话（全部项目）
func (r *SQLiteChatRepository) ListSessions(ctx context.Context, limit int) ([]types.ChatSessionRow, error) {
	return r.listSessions(ctx, "", limit)
}

// ListProjectSessions 列出某项目下最近的会话。
func (r *SQLiteChatRepository) ListProjectSessions(ctx context.Context, projectID string, limit int) ([]types.ChatSessionRow, error) {
	return r.listSessions(ctx, projectID, limit)
}

// listSessions projectID 为空 = 不按项目过滤。
func (r *SQLiteChatRepository) listSessions(ctx context.Context, projectID string, limit int) ([]types.ChatSessionRow, error) {
	q := `SELECT cs.id, cs.title, cs.project_id, cs.thrashing_index, cs.created_at, cs.updated_at, COUNT(cm.id) AS message_count
		FROM chat_sessions cs
		LEFT JOIN chat_messages cm ON cm.session_id = cs.id `
	args := []any{}
	if projectID != "" {
		q += `WHERE cs.project_id = ? `
		args = append(args, projectID)
	}
	q += `GROUP BY cs.id ORDER BY cs.updated_at DESC LIMIT ?`
	args = append(args, limit)

	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteChatRepository.ListSessions", err)
	}
	defer rows.Close()

	var result []types.ChatSessionRow
	for rows.Next() {
		var row types.ChatSessionRow
		if err := rows.Scan(&row.ID, &row.Title, &row.ProjectID, &row.ThrashingIndex, &row.CreatedAt, &row.UpdatedAt, &row.MessageCount); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteChatRepository.ListSessions scan", err)
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteChatRepository.ListSessions rows", err)
	}
	return result, nil
}

// SetSessionProject 把会话移动到另一个项目。会话不存在 / 项目不存在均返回 CodeNotFound。
func (r *SQLiteChatRepository) SetSessionProject(ctx context.Context, sessionID, projectID string) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE chat_sessions SET project_id=? WHERE id=?`, projectID, sessionID)
	if err != nil {
		if strings.Contains(err.Error(), "FOREIGN KEY") {
			return apperr.Wrap(apperr.CodeNotFound, "project not found", err)
		}
		return apperr.Wrap(apperr.CodeInternal, "SQLiteChatRepository.SetSessionProject", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return apperr.New(apperr.CodeNotFound, "session not found")
	}
	return nil
}

// UpdateSessionTitle 更新会话标题
func (r *SQLiteChatRepository) UpdateSessionTitle(ctx context.Context, id, title string) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE chat_sessions SET title=?, updated_at=strftime('%Y-%m-%dT%H:%M:%SZ','now') WHERE id=?`,
		title, id)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteChatRepository.UpdateSessionTitle", err)
	}
	return nil
}

// UpdateSessionThrashingIndex 更新 thrashing_index
func (r *SQLiteChatRepository) UpdateSessionThrashingIndex(ctx context.Context, id string, idx float64) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE chat_sessions SET thrashing_index=?, updated_at=strftime('%Y-%m-%dT%H:%M:%SZ','now') WHERE id=?`,
		idx, id)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteChatRepository.UpdateSessionThrashingIndex", err)
	}
	return nil
}

// DeleteSession 删除会话及其消息
func (r *SQLiteChatRepository) DeleteSession(ctx context.Context, id string) error {
	// session_todos 无指向 chat_sessions 的外键（子 Agent 会话没有该行），
	// 不会随级联删除，需同事务显式清理（ADR-0104 决策三）。
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteChatRepository.DeleteSession", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM session_todos WHERE session_id=?`, id); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteChatRepository.DeleteSession todos", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM chat_sessions WHERE id=?`, id); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteChatRepository.DeleteSession", err)
	}
	if err := tx.Commit(); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteChatRepository.DeleteSession commit", err)
	}
	return nil
}

// AppendMessage 追加一条消息。
//
// 修复：原实现的 INSERT 语句恒定使用 strftime('now') 作为 created_at/updated_at，
// 完全忽略调用方传入的 row.CreatedAt/row.UpdatedAt——ChatHandler.SaveMessage
// 为还原"回复实际耗时"特意回算了 created_at（now - durationMs），但因这里从未
// 使用这两个字段，回算逻辑此前是死代码，chat_messages.created_at 永远等于写入
// 时刻而非推理起始时刻。现在用 COALESCE + NULLIF 判空字符串回退 strftime(now)：
// 调用方未设置（如 durationMs<=0 场景）时保持原有写入即当前时间行为，
// 非空时采用调用方回算值。
// AppendMessage 返回新插入行的 chat_messages.id（AUTOINCREMENT），供调用方
// （SaveMessage）在需要时回填 MCP Apps 视图关联（M8f-1，见 LinkAppViewsToMessage）。
func (r *SQLiteChatRepository) AppendMessage(ctx context.Context, row types.ChatMessageRow) (int64, error) {
	id, err := r.appendMessage(ctx, "INSERT", row)
	if err != nil {
		return 0, apperr.Wrap(apperr.CodeInternal, "SQLiteChatRepository.AppendMessage", err)
	}
	return id, nil
}

// AppendMessageIdempotent 与 AppendMessage 相同，但使用 INSERT OR IGNORE 并要求
// row.DedupeKey 非空（GD-13-004 复核修复：outbox 重试兜底路径专用，OutboxWorker
// at-least-once 语义下可能对同一条记录多次调用 handler，dedupe_key 唯一索引
// 保证重复调用不会插入重复消息行）。该路径不需要消息 ID（调用方是异步 worker，
// 与"本轮产生的视图"这个上下文早已脱节），保持原 error-only 签名。
func (r *SQLiteChatRepository) AppendMessageIdempotent(ctx context.Context, row types.ChatMessageRow) error {
	if row.DedupeKey == "" {
		return apperr.New(apperr.CodeInvalidInput, "SQLiteChatRepository.AppendMessageIdempotent: dedupe_key required")
	}
	if _, err := r.appendMessage(ctx, "INSERT OR IGNORE", row); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteChatRepository.AppendMessageIdempotent", err)
	}
	return nil
}

func (r *SQLiteChatRepository) appendMessage(ctx context.Context, verb string, row types.ChatMessageRow) (int64, error) {
	var dedupeKey any
	if row.DedupeKey != "" {
		dedupeKey = row.DedupeKey
	}
	res, err := r.db.ExecContext(ctx,
		verb+` INTO chat_messages(session_id, role, content, reasoning_content, tool_calls, file_offset, file_length, dedupe_key, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?,?,
			COALESCE(NULLIF(?, ''), strftime('%Y-%m-%dT%H:%M:%SZ','now')),
			COALESCE(NULLIF(?, ''), strftime('%Y-%m-%dT%H:%M:%SZ','now')))`,
		row.SessionID, row.Role, row.Content, row.ReasoningContent, row.ToolCalls, row.FileOffset, row.FileLength, dedupeKey,
		row.CreatedAt, row.UpdatedAt)
	if err != nil {
		return 0, apperr.Wrap(apperr.CodeInternal, "SQLiteChatRepository.appendMessage", err)
	}
	// INSERT OR IGNORE 因 dedupe_key 冲突未插入行时 LastInsertId 返回 0（RowsAffected
	// 亦为 0），调用方（LinkAppViewsToMessage 场景）据此判断跳过回填，不是错误路径。
	id, idErr := res.LastInsertId()
	if idErr != nil {
		return 0, apperr.Wrap(apperr.CodeInternal, "SQLiteChatRepository.appendMessage: last_insert_id", idErr)
	}
	return id, nil
}

// ListMessages 列出指定会话的消息。limit<=0 表示不限行数（SQLite LIMIT -1）。
func (r *SQLiteChatRepository) ListMessages(ctx context.Context, sessionID string, limit int) ([]types.ChatMessageRow, error) {
	if limit <= 0 {
		limit = -1 // SQLite: LIMIT -1 = no upper bound
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, session_id, role, content, reasoning_content, tool_calls, file_offset, file_length, created_at, updated_at
		FROM chat_messages WHERE session_id=? ORDER BY id ASC LIMIT ?`, sessionID, limit)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteChatRepository.ListMessages", err)
	}
	defer rows.Close()

	var result []types.ChatMessageRow
	for rows.Next() {
		var row types.ChatMessageRow
		if err := rows.Scan(&row.ID, &row.SessionID, &row.Role, &row.Content, &row.ReasoningContent, &row.ToolCalls, &row.FileOffset, &row.FileLength, &row.CreatedAt, &row.UpdatedAt); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteChatRepository.ListMessages scan", err)
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteChatRepository.ListMessages rows", err)
	}
	return result, nil
}

// SearchMessages 全文检索消息
func (r *SQLiteChatRepository) SearchMessages(ctx context.Context, query string, limit int) ([]types.ChatMessageRow, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT cm.id, cm.session_id, cm.role, cm.content, cm.tool_calls, cm.file_offset, cm.file_length, cm.created_at, cm.updated_at
		FROM messages_fts fts
		JOIN chat_messages cm ON cm.id = fts.rowid
		WHERE messages_fts MATCH ? ORDER BY rank LIMIT ?`, util.QuoteFTS5Query(query), limit)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteChatRepository.SearchMessages", err)
	}
	defer rows.Close()

	var result []types.ChatMessageRow
	for rows.Next() {
		var row types.ChatMessageRow
		if err := rows.Scan(&row.ID, &row.SessionID, &row.Role, &row.Content, &row.ToolCalls, &row.FileOffset, &row.FileLength, &row.CreatedAt, &row.UpdatedAt); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteChatRepository.SearchMessages scan", err)
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteChatRepository.SearchMessages rows", err)
	}
	return result, nil
}

// --- Additional mutations ---

// RestoreSession 备份恢复。projectID 在本库不存在（旧备份无该字段 / 项目未随备份恢复）
// 时落默认项目，而不是因外键失败丢掉整个会话。
func (r *SQLiteChatRepository) RestoreSession(ctx context.Context, id, title, projectID string, thrashing float64, createdAt, updatedAt string) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO chat_sessions(id, title, project_id, thrashing_index, created_at, updated_at)
		 VALUES(?, ?, COALESCE((SELECT id FROM projects WHERE id = ?), ?), ?, ?, ?)`,
		id, title, projectID, protorepo.DefaultProjectID, thrashing, createdAt, updatedAt)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "db error", err)
	}
	return nil
}

func (r *SQLiteChatRepository) RestoreMessage(ctx context.Context, id, sessionID, role, content, createdAt string) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO chat_messages(id, session_id, role, content, created_at) VALUES(?,?,?,?,?)`,
		id, sessionID, role, content, createdAt)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "db error", err)
	}
	return nil
}

func (r *SQLiteChatRepository) TouchSession(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE chat_sessions SET updated_at=datetime('now') WHERE id=?`, id)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "db error", err)
	}
	return nil
}

func (r *SQLiteChatRepository) ClearNonSystemMessages(ctx context.Context, sessionID string) error {
	_, err := r.db.ExecContext(ctx,
		`DELETE FROM chat_messages WHERE session_id=? AND role != 'system'`, sessionID)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "db error", err)
	}
	return nil
}

// ── MCP Apps（M8f-1）: chat_app_views + chat_sessions.app_model_context ──────

// SaveAppView 落库一次工具调用产生的 UI 视图快照。message_id 落 NULL——tool_ui
// 事件产生于 FSM 工具执行期，早于本轮 assistant 消息落库，见 LinkAppViewsToMessage。
func (r *SQLiteChatRepository) SaveAppView(ctx context.Context, v types.ChatAppViewRow) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO chat_app_views(view_id, session_id, server_id, resource_uri, tool_name, tool_input, tool_result, widget_state)
		 VALUES(?,?,?,?,?,?,?,?)`,
		v.ViewID, v.SessionID, v.ServerID, v.ResourceURI, v.ToolName, v.ToolInput, v.ToolResult, v.WidgetState)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteChatRepository.SaveAppView", err)
	}
	return nil
}

// LinkAppViewsToMessage 把本轮产生的全部视图关联到刚落库的 assistant 消息（同一
// sessionID 二次校验，防止极端时序下把视图错误关联到并发的另一会话消息）。
func (r *SQLiteChatRepository) LinkAppViewsToMessage(ctx context.Context, sessionID string, viewIDs []string, messageID int64) error {
	if len(viewIDs) == 0 {
		return nil
	}
	placeholders := strings.Repeat("?,", len(viewIDs))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, 0, len(viewIDs)+2)
	args = append(args, messageID, sessionID)
	for _, id := range viewIDs {
		args = append(args, id)
	}
	_, err := r.db.ExecContext(ctx,
		`UPDATE chat_app_views SET message_id=? WHERE session_id=? AND view_id IN (`+placeholders+`)`, args...)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteChatRepository.LinkAppViewsToMessage", err)
	}
	return nil
}

// GetAppView 按 view_id 查询单条视图；不存在返回 (nil, nil)（与 GetSession 同一约定）。
func (r *SQLiteChatRepository) GetAppView(ctx context.Context, viewID string) (*types.ChatAppViewRow, error) {
	var v types.ChatAppViewRow
	var messageID sql.NullInt64
	err := r.db.QueryRowContext(ctx,
		`SELECT view_id, session_id, message_id, server_id, resource_uri, tool_name, tool_input, tool_result, widget_state, created_at
		 FROM chat_app_views WHERE view_id=?`, viewID,
	).Scan(&v.ViewID, &v.SessionID, &messageID, &v.ServerID, &v.ResourceURI, &v.ToolName, &v.ToolInput, &v.ToolResult, &v.WidgetState, &v.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteChatRepository.GetAppView", err)
	}
	if messageID.Valid {
		v.MessageID = &messageID.Int64
	}
	return &v, nil
}

// ListAppViewsByMessageIDs 批量查询一组 assistant 消息关联的视图（会话历史接口，
// 每条助手消息渲染其关联的全部视图）。messageIDs 为空返回空切片。
func (r *SQLiteChatRepository) ListAppViewsByMessageIDs(ctx context.Context, messageIDs []int64) ([]types.ChatAppViewRow, error) {
	if len(messageIDs) == 0 {
		return nil, nil
	}
	placeholders := strings.Repeat("?,", len(messageIDs))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, 0, len(messageIDs))
	for _, id := range messageIDs {
		args = append(args, id)
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT view_id, session_id, message_id, server_id, resource_uri, tool_name, tool_input, tool_result, widget_state, created_at
		 FROM chat_app_views WHERE message_id IN (`+placeholders+`) ORDER BY created_at ASC`, args...)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteChatRepository.ListAppViewsByMessageIDs", err)
	}
	defer rows.Close()

	var result []types.ChatAppViewRow
	for rows.Next() {
		var v types.ChatAppViewRow
		var messageID sql.NullInt64
		if err := rows.Scan(&v.ViewID, &v.SessionID, &messageID, &v.ServerID, &v.ResourceURI, &v.ToolName, &v.ToolInput, &v.ToolResult, &v.WidgetState, &v.CreatedAt); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteChatRepository.ListAppViewsByMessageIDs scan", err)
		}
		if messageID.Valid {
			v.MessageID = &messageID.Int64
		}
		result = append(result, v)
	}
	if err := rows.Err(); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SQLiteChatRepository.ListAppViewsByMessageIDs rows", err)
	}
	return result, nil
}

// UpdateAppViewWidgetState 持久化 ChatGPT widgetState 兼容字段（大小上限由调用方
// 网关 handler 校验，仓储层不重复判断）。
func (r *SQLiteChatRepository) UpdateAppViewWidgetState(ctx context.Context, viewID, widgetState string) error {
	res, err := r.db.ExecContext(ctx, `UPDATE chat_app_views SET widget_state=? WHERE view_id=?`, widgetState, viewID)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteChatRepository.UpdateAppViewWidgetState", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return apperr.New(apperr.CodeNotFound, "mcp apps view not found: "+viewID)
	}
	return nil
}

// appModelContextPath SQLite JSON path，按 server_id 为键定位 app_model_context
// 顶层对象的一个字段。作为绑定参数传给 json_set（而非拼进 SQL 文本），
// server_id 中的双引号转义后即为安全的 JSON path 片段。
func appModelContextPath(serverID string) string {
	return `$."` + strings.ReplaceAll(serverID, `"`, `\"`) + `"`
}

// UpsertSessionModelContextServer 按 server_id 为键合并一份内容到
// chat_sessions.app_model_context（每 (session, server) 只保留最新一份，
// apps_spec.mdx §MCP Apps Specific Messages："Each request overwrites the
// previous context sent by the View"）。
func (r *SQLiteChatRepository) UpsertSessionModelContextServer(ctx context.Context, sessionID, serverID, payloadJSON string) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE chat_sessions SET app_model_context = json_set(app_model_context, ?, json(?)) WHERE id=?`,
		appModelContextPath(serverID), payloadJSON, sessionID)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteChatRepository.UpsertSessionModelContextServer", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return apperr.New(apperr.CodeNotFound, "session not found: "+sessionID)
	}
	return nil
}

// ConsumeSessionModelContext 读取并清空该会话全部待注入的 model-context（规范
// 要求注入后清空，"只把最后一次更新送给模型"）。会话不存在返回 ("{}", nil)——
// 由调用方（PromptAssembler）决定是否是错误，本层只负责数据存在性事实。
func (r *SQLiteChatRepository) ConsumeSessionModelContext(ctx context.Context, sessionID string) (string, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "SQLiteChatRepository.ConsumeSessionModelContext begin", err)
	}
	defer tx.Rollback() //nolint:errcheck

	var content string
	err = tx.QueryRowContext(ctx, `SELECT app_model_context FROM chat_sessions WHERE id=?`, sessionID).Scan(&content)
	if errors.Is(err, sql.ErrNoRows) {
		return "{}", nil
	}
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "SQLiteChatRepository.ConsumeSessionModelContext select", err)
	}
	if content == "" || content == "{}" {
		return "{}", nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE chat_sessions SET app_model_context='{}' WHERE id=?`, sessionID); err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "SQLiteChatRepository.ConsumeSessionModelContext clear", err)
	}
	if err := tx.Commit(); err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "SQLiteChatRepository.ConsumeSessionModelContext commit", err)
	}
	return content, nil
}

func (r *SQLiteChatRepository) ReplaceSessionMessages(ctx context.Context, sessionID string, msgs []types.ChatMessageRow) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "db error", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.ExecContext(ctx, `DELETE FROM chat_messages WHERE session_id=?`, sessionID); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "db error", err)
	}
	for _, m := range msgs {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO chat_messages(session_id, role, content) VALUES(?,?,?)`,
			sessionID, m.Role, m.Content); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "db error", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SQLiteChatRepository.ReplaceSessionMessages commit", err)
	}
	return nil
}
