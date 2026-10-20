package chat

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/polarisagi/polaris/internal/gateway/httputil"
	"github.com/polarisagi/polaris/internal/gateway/session"
	"github.com/polarisagi/polaris/internal/protocol/repo"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// 回合步骤时间线的存取（ADR-0118）：session.TurnStore 的适配器 + 懒加载接口。

// turnStore 把 repo 层适配成 session.TurnStore（session 包零 repo 依赖，HE-3）。
type turnStore struct {
	steps repo.ChatTurnRepository
	usage repo.LLMSessionUsageRepository
}

// newTurnStore 任一依赖缺失返回 nil 接口（而非"非 nil 接口包 nil 指针"），让 session 侧的 nil 判空生效。
func newTurnStore(steps repo.ChatTurnRepository, usage repo.LLMSessionUsageRepository) session.TurnStore {
	if steps == nil || usage == nil {
		return nil
	}
	return &turnStore{steps: steps, usage: usage}
}

func (t *turnStore) AppendTurnSteps(ctx context.Context, sessionID, turnID string, recs []session.StepRecord) error {
	rows := make([]repo.TurnStepRow, 0, len(recs))
	for _, r := range recs {
		rows = append(rows, repo.TurnStepRow{
			TurnID: turnID, SessionID: sessionID, Seq: r.Seq, ParentID: r.ParentID,
			Kind: r.Kind, Data: r.Data, CreatedAtMs: r.CreatedAtMs,
		})
	}
	return t.steps.AppendTurnSteps(ctx, rows) //nolint:wrapcheck // repo 层已带上下文
}

func (t *turnStore) TurnUsage(ctx context.Context, turnID string) (session.TurnUsageSummary, error) {
	u, err := t.usage.TurnUsage(ctx, turnID)
	if err != nil {
		return session.TurnUsageSummary{}, err //nolint:wrapcheck // 同上
	}
	return session.TurnUsageSummary{Calls: u.Calls, Input: u.InputTokens, CacheHit: u.CacheHitTokens, Output: u.OutputTokens, CostUSD: u.CostUSD}, nil
}

func (t *turnStore) AttachTurnMeta(ctx context.Context, messageID int64, turnID, usageJSON string) error {
	return t.steps.AttachTurnMeta(ctx, messageID, turnID, usageJSON) //nolint:wrapcheck // 同上
}

// turnSummaries 会话内各回合的步骤摘要（turn_id → 摘要 JSON）。未接线或查询失败按"没有步骤"处理：
// 步骤是展示增强，不该拖垮历史消息本身的加载。
func (h *ChatHandler) turnSummaries(ctx context.Context, sessionID string) map[string]json.RawMessage {
	if h.TurnRepo == nil {
		return nil
	}
	raw, err := h.TurnRepo.TurnSummaries(ctx, sessionID)
	if err != nil || len(raw) == 0 {
		return nil
	}
	out := make(map[string]json.RawMessage, len(raw))
	for k, v := range raw {
		out[k] = json.RawMessage(v)
	}
	return out
}

// GET /v1/sessions/{sessionID}/turns/{turnID}/steps
// 展开时才加载一个回合的全部步骤（会话加载只带摘要，不带步骤本体）。
func (h *ChatHandler) HandleGetTurnSteps(w http.ResponseWriter, r *http.Request) {
	sessionID := strings.TrimSpace(r.PathValue("sessionID"))
	turnID := strings.TrimSpace(r.PathValue("turnID"))
	if !session.SessionIDPattern.MatchString(sessionID) || !validTurnID(turnID) {
		httputil.RespondError(w, "", apperr.New(apperr.CodeInvalidInput, "invalid session_id or turn_id"), http.StatusBadRequest)
		return
	}
	if h.TurnRepo == nil {
		httputil.RespondError(w, "turn steps unavailable", nil, http.StatusServiceUnavailable)
		return
	}
	rows, err := h.TurnRepo.ListTurnSteps(r.Context(), sessionID, turnID)
	if err != nil {
		httputil.RespondError(w, "", err, http.StatusInternalServerError)
		return
	}
	steps := make([]json.RawMessage, 0, len(rows))
	var summary json.RawMessage
	for _, row := range rows {
		if row.Seq == 0 {
			summary = json.RawMessage(row.Data)
			continue
		}
		steps = append(steps, json.RawMessage(row.Data))
	}
	httputil.WriteJSON(w, map[string]any{"session_id": sessionID, "turn_id": turnID, "summary": summary, "steps": steps})
}

// validTurnID 回合 ID 白名单：turn_ 前缀 + 十六进制/字母数字，或内核 uuid（测试与旧路径）。
func validTurnID(id string) bool {
	if id == "" || len(id) > 80 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}
