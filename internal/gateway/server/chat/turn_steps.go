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

// SessionToolSteps 会话内各回合的工具步骤（turn_id → 步骤，按 seq 升序），供回合摘要（ADR-0120 决策四）。
// 解析失败的行跳过：摘要是增强，不能因一行坏数据拖垮回合。
func (t *turnStore) SessionToolSteps(ctx context.Context, sessionID string) (map[string][]session.Step, error) {
	rows, err := t.steps.ListSessionToolSteps(ctx, sessionID)
	if err != nil {
		return nil, err //nolint:wrapcheck // repo 层已带上下文
	}
	out := make(map[string][]session.Step)
	for _, r := range rows {
		var st session.Step
		if json.Unmarshal(r.Data, &st) != nil {
			continue
		}
		out[r.TurnID] = append(out[r.TurnID], st)
	}
	return out, nil
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
			summary = safeStepSummary(row.Data)
			continue
		}
		steps = append(steps, safeStepRecord(row.Data))
	}
	httputil.WriteJSON(w, map[string]any{"session_id": sessionID, "turn_id": turnID, "summary": summary, "steps": steps})
}

// validTurnID 回合 ID 白名单：turn_ 前缀 + 十六进制/字母数字，或内核 uuid（测试与旧路径）。
func validTurnID(id string) bool {
	if id == "" || len(id) > 80 {
		return false
	}
	for _, c := range id {
		isAlnum := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
		if !isAlnum && c != '_' && c != '-' {
			return false
		}
	}
	return true
}

// failedTurnError 从回合摘要行取出失败信息：摘要未标记失败（或不可解析）返回 nil。
func failedTurnError(summary json.RawMessage) json.RawMessage {
	if len(summary) == 0 {
		return nil
	}
	var s struct {
		Failed     bool   `json:"failed"`
		ErrorCode  string `json:"error_code"`
		ErrorClass string `json:"error_class"`
		Error      string `json:"error"`
	}
	if json.Unmarshal(summary, &s) != nil || !s.Failed {
		return nil
	}
	kind := safeFailureKind(s.ErrorCode)
	out, err := json.Marshal(map[string]string{
		"code":    string(apperr.NormalizeErrorCode(apperr.Code(s.ErrorClass))),
		"kind":    kind,
		"message": safeFailureMessage(kind),
	})
	if err != nil {
		return nil
	}
	return out
}

func safeStepSummary(raw []byte) json.RawMessage {
	var shape map[string]json.RawMessage
	if json.Unmarshal(raw, &shape) != nil || shape == nil {
		return json.RawMessage(`{}`)
	}
	var summary session.StepSummary
	if json.Unmarshal(raw, &summary) != nil {
		return json.RawMessage(`{}`)
	}
	hasFailureMetadata := summary.Failed || summary.Error != "" || summary.ErrorCode != "" || summary.ErrorClass != ""
	if !hasFailureMetadata && len(shape["error_detail"]) == 0 && len(shape["error_class"]) == 0 {
		safe, err := json.Marshal(summary)
		if err != nil {
			return json.RawMessage(`{}`)
		}
		return safe
	}
	var projection map[string]any
	encoded, err := json.Marshal(summary)
	if err != nil || json.Unmarshal(encoded, &projection) != nil {
		return json.RawMessage(`{}`)
	}
	delete(projection, "error_detail")
	delete(projection, "error_class")
	if hasFailureMetadata {
		kind := safeFailureKind(summary.ErrorCode)
		projection["kind"] = kind
		projection["error_code"] = string(apperr.NormalizeErrorCode(apperr.Code(summary.ErrorClass)))
		projection["error"] = safeFailureMessage(kind)
	}
	safe, err := json.Marshal(projection)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return safe
}

func safeStepRecord(raw []byte) json.RawMessage {
	var shape map[string]json.RawMessage
	if json.Unmarshal(raw, &shape) != nil || shape == nil {
		return json.RawMessage(`{}`)
	}
	var step session.Step
	if json.Unmarshal(raw, &step) != nil {
		return json.RawMessage(`{}`)
	}
	if step.Kind != session.StepKindError && step.Status != session.StepStatusError &&
		step.Status != session.StepStatusDenied && step.Status != session.StepStatusCancelled {
		safe, err := json.Marshal(step)
		if err != nil {
			return json.RawMessage(`{}`)
		}
		return safe
	}
	if step.Kind == session.StepKindError {
		step.Title = "模型调用失败"
	}
	step.ResultPreview = safeFailedStepMessage(step)
	safe, err := json.Marshal(step)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return safe
}

func safeFailureKind(kind string) string {
	switch kind {
	case "session_error", "history_error", "agent_pool_error", "empty_response", "fsm_error",
		"hook_blocked", "no_provider", "no_agent":
		return kind
	default:
		return "turn_error"
	}
}

func safeFailureMessage(kind string) string {
	switch safeFailureKind(kind) {
	case "hook_blocked":
		return "请求被安全策略拦截"
	case "no_provider":
		return "当前没有可用模型，请检查模型配置"
	case "no_agent":
		return "会话服务暂不可用，请稍后重试"
	case "session_error":
		return "会话初始化失败，请稍后重试"
	case "history_error":
		return "会话历史读取失败，请稍后重试"
	case "agent_pool_error":
		return "会话 Agent 暂不可用，请稍后重试"
	case "empty_response", "fsm_error":
		return "模型调用失败，请稍后重试"
	default:
		return "本轮执行失败，请稍后重试"
	}
}

func safeFailedStepMessage(step session.Step) string {
	if step.Kind == session.StepKindError {
		return "模型调用失败，请稍后重试"
	}
	switch step.Status {
	case session.StepStatusDenied:
		return "操作未获批准"
	case session.StepStatusCancelled:
		return "操作已取消"
	default:
		return "工具执行失败"
	}
}

// turnMeta 决定一条历史消息携带的回合信息。用户消息上的 turn_id 只表示"这一轮失败了"
// （失败占位），成功回合的 turn_id 与步骤摘要只挂在助手消息上。
func turnMeta(role, turnID string, summaries map[string]json.RawMessage) (id string, steps, turnErr json.RawMessage) {
	if turnID == "" {
		return "", nil, nil
	}
	if role != "user" {
		return turnID, safeStepSummary(summaries[turnID]), nil
	}
	if te := failedTurnError(summaries[turnID]); te != nil {
		return turnID, nil, te
	}
	return "", nil, nil
}
