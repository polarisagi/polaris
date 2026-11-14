package chat

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/gateway/httputil"
	"github.com/polarisagi/polaris/pkg/apperr"
	apptypes "github.com/polarisagi/polaris/pkg/types"
)

// ─── 会话 CRUD HTTP 处理器 ──────────────────────────────────────────────────

// GET /v1/sessions
func (h *ChatHandler) HandleListSessions(w http.ResponseWriter, r *http.Request) {
	limit := sessionListLimit(r.URL.Query().Get("limit"))
	sessions, err := h.listSessionRows(r.Context(), strings.TrimSpace(r.URL.Query().Get("project_id")), limit)
	if err != nil {
		httputil.RespondError(w, "", err, http.StatusInternalServerError)
		return
	}

	running := h.turns().RunningSet()
	channelTypes := h.sessionChannelTypes(r.Context())
	list := make([]sessionListDTO, 0, len(sessions))
	for _, row := range sessions {
		list = append(list, sessionListItem(row, running[row.ID], channelTypes))
	}
	if list == nil {
		list = []sessionListDTO{}
	}
	httputil.WriteJSON(w, map[string]any{"sessions": list})
}

type sessionListDTO struct {
	ID                 string          `json:"id"`
	Title              string          `json:"title"`
	ProjectID          string          `json:"project_id"`
	ThrashingIndex     float64         `json:"thrashing_index"`
	CreatedAt          string          `json:"created_at"`
	UpdatedAt          string          `json:"updated_at"`
	MessageCount       int             `json:"message_count"`
	Source             string          `json:"source"`
	Running            bool            `json:"running"`
	AgentProfile       string          `json:"agent_profile,omitempty"`
	AgentProfileSource string          `json:"agent_profile_source,omitempty"`
	AgentProfileDigest string          `json:"agent_profile_digest,omitempty"`
	ExpertPluginID     string          `json:"expert_plugin_id,omitempty"`
	ExpertID           string          `json:"expert_id,omitempty"`
	ExpertType         string          `json:"expert_type,omitempty"`
	ExpertDisplayName  json.RawMessage `json:"expert_display_name,omitempty"`
}

func sessionListLimit(raw string) int {
	if n, err := strconv.Atoi(raw); err == nil && n >= 1 && n <= 200 {
		return n
	}
	return 200
}

func (h *ChatHandler) listSessionRows(ctx context.Context, projectID string, limit int) ([]apptypes.ChatSessionRow, error) {
	if projectID != "" {
		rows, err := h.PersistenceService.ChatRepo.ListProjectSessions(ctx, projectID, limit)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeOf(err), "chat list project sessions", err)
		}
		return rows, nil
	}
	rows, err := h.PersistenceService.ChatRepo.ListSessions(ctx, limit)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeOf(err), "chat list sessions", err)
	}
	return rows, nil
}

func (h *ChatHandler) sessionChannelTypes(ctx context.Context) map[string]string {
	out := map[string]string{}
	if h.ChannelRepo == nil {
		return out
	}
	rows, err := h.ChannelRepo.ListChannels(ctx)
	if err != nil {
		return out
	}
	for _, row := range rows {
		out[row.ID] = row.Type
	}
	return out
}

func sessionListItem(row apptypes.ChatSessionRow, running bool, channelTypes map[string]string) sessionListDTO {
	dto := sessionListDTO{ID: row.ID, Title: row.Title, ProjectID: row.ProjectID, ThrashingIndex: row.ThrashingIndex,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt, MessageCount: row.MessageCount, Source: "web", Running: running,
		AgentProfile: row.AgentProfile, AgentProfileSource: row.AgentProfileSource, AgentProfileDigest: row.AgentProfileDigest,
		ExpertPluginID: row.ExpertPluginID, ExpertID: row.ExpertID, ExpertType: row.ExpertType}
	if json.Valid([]byte(row.ExpertDisplayName)) && row.ExpertDisplayName != "{}" {
		dto.ExpertDisplayName = json.RawMessage(row.ExpertDisplayName)
	}
	if channelID, ok := strings.CutPrefix(row.ID, "ch_"); ok {
		for id, channelType := range channelTypes {
			if strings.HasPrefix(channelID, id+"_") {
				dto.Source = channelType
				break
			}
		}
		if dto.Source == "web" {
			dto.Source = "channel"
		}
	}
	return dto
}

// GET /v1/sessions/{sessionID}
func (h *ChatHandler) HandleGetSession(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("sessionID")
	maxChars := 50000
	if v := r.URL.Query().Get("max_chars"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			maxChars = n
		}
	}

	messages, err := h.PersistenceService.ChatRepo.ListMessages(r.Context(), sessionID, 100000)
	if err != nil {
		httputil.RespondError(w, "", err, http.StatusInternalServerError)
		return
	}

	type msgRow struct {
		Role             string          `json:"role"`
		Content          string          `json:"content"`
		ReasoningContent string          `json:"reasoning_content,omitempty"`
		ToolCalls        json.RawMessage `json:"tool_calls,omitempty"`
		TaskDuration     int64           `json:"task_duration,omitempty"` // in ms
		// Views 本条助手消息关联的 MCP Apps UI 视图（M8f-1），刷新页面后前端
		// 据此重新渲染。只有 role="assistant" 且本轮产生过 tool_ui 事件的消息非空。
		Views []mcpAppViewDTO `json:"views,omitempty"`
		// TurnID/Usage/Steps（ADR-0118）：产生本条助手消息的回合、用量摘要、步骤摘要（步数/工具数/用时/token）。
		// 步骤本体不随历史返回，展开时按 TurnID 走 GET /v1/sessions/{id}/turns/{turn_id}/steps 懒加载。
		TurnID string          `json:"turn_id,omitempty"`
		Usage  json.RawMessage `json:"usage,omitempty"`
		Steps  json.RawMessage `json:"steps,omitempty"`
		// TurnError 仅用户消息携带：该消息触发的回合失败（没有助手回复）。前端据此在其后显示"本轮失败"占位。
		// 来自回合步骤摘要，只用于展示；不属于 chat_messages 内容，永远不进入模型历史。
		TurnError json.RawMessage `json:"turn_error,omitempty"`
	}
	viewsByMessage := h.appViewsByMessageID(r.Context(), messages)
	stepSummaries := h.turnSummaries(r.Context(), sessionID)
	msgs := make([]msgRow, 0, len(messages))
	total := 0
	for _, row := range messages {
		m := msgRow{
			Role:             row.Role,
			Content:          row.Content,
			ReasoningContent: row.ReasoningContent,
			TaskDuration:     parseTaskDuration(row.CreatedAt, row.UpdatedAt),
			Views:            viewsByMessage[row.ID],
		}
		if row.ToolCalls != "" {
			m.ToolCalls = json.RawMessage(row.ToolCalls)
		}
		m.TurnID, m.Steps, m.TurnError = turnMeta(row.Role, row.TurnID, stepSummaries)
		if row.Usage != "" && json.Valid([]byte(row.Usage)) {
			m.Usage = json.RawMessage(row.Usage)
		}

		total += len(m.Content)
		if total > maxChars {
			break
		}
		msgs = append(msgs, m)
	}
	if msgs == nil {
		msgs = []msgRow{}
	}
	// project_id 供前端恢复会话时同步"当前项目"（ADR-0097）；会话行不存在时省略。
	resp := map[string]any{"session_id": sessionID, "messages": msgs, "running": h.turns().Running(sessionID)}
	binding, gerr := h.sessionBinding(r.Context(), sessionID)
	if gerr != nil {
		httputil.RespondError(w, "session lookup failed", gerr, apperr.HTTPStatus(apperr.CodeOf(gerr)))
		return
	}
	for key, value := range binding {
		resp[key] = value
	}
	httputil.WriteJSON(w, resp)
}

func (h *ChatHandler) sessionBinding(ctx context.Context, id string) (map[string]any, error) {
	row, err := h.PersistenceService.ChatRepo.GetSession(ctx, id)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeOf(err), "chat get session binding", err)
	}
	if row == nil {
		return nil, nil
	}
	binding := map[string]any{"project_id": row.ProjectID, "agent_profile": row.AgentProfile}
	if row.AgentProfileSource != "" {
		binding["agent_profile_source"] = row.AgentProfileSource
		binding["agent_profile_digest"] = row.AgentProfileDigest
	}
	if row.ExpertID != "" {
		binding["expert_plugin_id"], binding["expert_id"], binding["expert_type"] = row.ExpertPluginID, row.ExpertID, row.ExpertType
		if json.Valid([]byte(row.ExpertDisplayName)) {
			binding["expert_display_name"] = json.RawMessage(row.ExpertDisplayName)
		}
	}
	return binding, nil
}

// mcpAppViewDTO 会话历史接口暴露的 MCP Apps UI 视图快照（M8f-1）。
type mcpAppViewDTO struct {
	ViewID      string          `json:"view_id"`
	ServerID    string          `json:"server_id"`
	ResourceURI string          `json:"resource_uri"`
	ToolName    string          `json:"tool_name"`
	ToolInput   json.RawMessage `json:"tool_input,omitempty"`
	ToolResult  json.RawMessage `json:"tool_result,omitempty"`
	WidgetState json.RawMessage `json:"widget_state,omitempty"`
}

// appViewsByMessageID 批量查询本次返回的消息集合关联的全部视图，按
// chat_messages.id 分组（M8f-1）。查询失败按无视图处理（记 Warn，不阻断历史加载——
// 会话历史的核心是文本消息本身，视图渲染是增强能力）。
func (h *ChatHandler) appViewsByMessageID(ctx context.Context, messages []apptypes.ChatMessageRow) map[int64][]mcpAppViewDTO {
	ids := make([]int64, 0, len(messages))
	for _, m := range messages {
		ids = append(ids, m.ID)
	}
	views, err := h.PersistenceService.ChatRepo.ListAppViewsByMessageIDs(ctx, ids)
	if err != nil {
		slog.Warn("chat: list app views by message ids failed", "err", err)
		return nil
	}
	byMsg := make(map[int64][]mcpAppViewDTO, len(views))
	for _, v := range views {
		if v.MessageID == nil {
			continue
		}
		byMsg[*v.MessageID] = append(byMsg[*v.MessageID], mcpAppViewDTO{
			ViewID: v.ViewID, ServerID: v.ServerID, ResourceURI: v.ResourceURI, ToolName: v.ToolName,
			ToolInput: json.RawMessage(v.ToolInput), ToolResult: json.RawMessage(v.ToolResult),
			WidgetState: json.RawMessage(v.WidgetState),
		})
	}
	return byMsg
}

func parseTaskDuration(createdStr, updatedStr string) int64 {
	if createdStr == "" || updatedStr == "" {
		return 0
	}
	parseDBTime := func(s string) time.Time {
		if t, err := time.Parse("2006-01-02 15:04:05", s); err == nil {
			return t
		}
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t
		}
		return time.Time{}
	}
	tC := parseDBTime(createdStr)
	tU := parseDBTime(updatedStr)
	if !tU.IsZero() && !tC.IsZero() {
		return tU.Sub(tC).Milliseconds()
	}
	return 0
}

// DELETE /v1/sessions/{sessionID}
func (h *ChatHandler) HandleDeleteSession(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("sessionID")
	if err := h.PersistenceService.ChatRepo.DeleteSession(r.Context(), sessionID); err != nil {
		httputil.RespondError(w, "", err, http.StatusInternalServerError)
		return
	}
	if h.Hooks != nil {
		h.Hooks.SessionEnd(r.Context(), sessionID, "other")
	}
	httputil.WriteJSON(w, map[string]string{"status": "deleted"})
}

// 会话辅助方法（EnsureSession/ListMessages/SaveMessage/UpdateSessionTitle/
// TouchSession/truncate）见 sessions_helpers.go；全文搜索与会话上下文统计
// 处理器见 sessions_search.go（均为 R7 拆分）。newSessionID 已随 A-03 Step2
// 迁入 internal/gateway/session/ids.go（会话 ID 生成收口至 session 包）。
