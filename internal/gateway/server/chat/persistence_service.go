package chat

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/polarisagi/polaris/internal/eval/analysis"
	"github.com/polarisagi/polaris/internal/gateway/session"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

type ChatPersistenceService struct {
	ChatRepo        protocol.ChatRepository
	DB              protocol.SQLQuerier
	OutboxWriter    protocol.OutboxWriter
	SamplingMonitor *analysis.ContinuousSamplingMonitor
	Registry        protocol.LLMRegistry
}

func NewChatPersistenceService(
	chatRepo protocol.ChatRepository,
	db protocol.SQLQuerier,
	outboxWriter protocol.OutboxWriter,
	samplingMonitor *analysis.ContinuousSamplingMonitor,
	registry protocol.LLMRegistry,
) *ChatPersistenceService {
	return &ChatPersistenceService{
		ChatRepo:        chatRepo,
		DB:              db,
		OutboxWriter:    outboxWriter,
		SamplingMonitor: samplingMonitor,
		Registry:        registry,
	}
}

func (s *ChatPersistenceService) EnsureSession(ctx context.Context, sessionID string) error {
	return s.EnsureSessionInProject(ctx, sessionID, "")
}

// EnsureSessionInProject 见 session.Persistence 同名方法（ADR-0097）。
// 保留 apperr 的 Code（NotFound=项目不存在），不再一律降级为 Internal。
func (s *ChatPersistenceService) EnsureSessionInProject(ctx context.Context, sessionID, projectID string) error {
	err := s.ChatRepo.CreateSession(ctx, types.ChatSessionRow{ID: sessionID, Title: "", ProjectID: projectID})
	if err != nil {
		// 项目不存在（NotFound）/ 项目已归档（InvalidInput）是用户可理解的拒绝，保留 Code。
		if code := apperr.CodeOf(err); code == apperr.CodeNotFound || code == apperr.CodeInvalidInput {
			return apperr.Wrap(code, "Server.ensureSession", err)
		}
		return apperr.Wrap(apperr.CodeInternal, "Server.ensureSession", err)
	}
	return nil
}

func (s *ChatPersistenceService) ListMessages(ctx context.Context, sessionID string) ([]types.Message, error) {
	rows, err := s.ChatRepo.ListMessages(ctx, sessionID, 0)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "Server.loadMessages", err)
	}

	var msgs []types.Message
	for _, r := range rows {
		msgs = append(msgs, types.Message{
			Role:             r.Role,
			Content:          r.Content,
			ReasoningContent: r.ReasoningContent,
		})
	}
	return msgs, nil
}

// SaveMessage 返回新插入行的 chat_messages.id；outbox 兜底路径（直接写入重试耗尽后
// 转异步）与直接写入本身失败（重试+outbox 均失败）两种情况下 messageID 为 0——
// 调用方（session.orchestrator）据此判断是否跳过 MCP Apps 视图关联回填
// （M8f-1，LinkAppViewsToMessage），这是可接受的降级：消息本身仍已持久化或已入队，
// 只是视图与消息的关联在这一次不成立，不是致命错误。
func (s *ChatPersistenceService) SaveMessage(ctx context.Context, sessionID, role, content string, toolCalls string, reasoningContent string, durationMs int64) (int64, error) {
	now := time.Now().UTC()
	createdAt := now.Format(time.RFC3339)
	if durationMs > 0 {
		createdAt = now.Add(-time.Duration(durationMs) * time.Millisecond).Format(time.RFC3339)
	}

	row := types.ChatMessageRow{
		SessionID:        sessionID,
		Role:             role,
		Content:          content,
		ReasoningContent: reasoningContent,
		ToolCalls:        toolCalls,
		DedupeKey:        newMessageDedupeKey(sessionID, role),
	}
	if durationMs > 0 {
		row.UpdatedAt = now.Format(time.RFC3339)
		row.CreatedAt = createdAt
	}

	var lastErr error
retryLoop:
	for attempt := range saveMessageRetryAttempts {
		if attempt > 0 {
			select {
			case <-time.After(saveMessageRetryBackoff(attempt - 1)):
			case <-ctx.Done():
				lastErr = ctx.Err()
				break retryLoop
			}
		}
		id, err := s.ChatRepo.AppendMessage(ctx, row)
		if err != nil {
			lastErr = err
			slog.Warn("server: saveMessage attempt failed, will retry", "session", sessionID, "role", role, "attempt", attempt+1, "err", err)
			continue
		}
		return id, nil
	}

	if s.OutboxWriter != nil {
		payload := chatMessagePersistPayload{
			SessionID:        row.SessionID,
			Role:             row.Role,
			Content:          row.Content,
			ReasoningContent: row.ReasoningContent,
			ToolCalls:        row.ToolCalls,
			CreatedAt:        row.CreatedAt,
			UpdatedAt:        row.UpdatedAt,
			DedupeKey:        row.DedupeKey,
		}
		payloadBytes, marshalErr := json.Marshal(payload)
		if marshalErr == nil {
			entry := protocol.OutboxEntry{
				TargetEngine: protocol.TopicChatMessagePersistRetry,
				Operation:    "insert",
				Payload:      payloadBytes,
				IdempotencyKey: string(types.BuildIdempotencyKey(protocol.TopicChatMessagePersistRetry, "chat_message",
					row.DedupeKey, "insert", 1)),
			}
			obCtx, obCancel := context.WithTimeout(context.Background(), 3*time.Second)
			writeErr := s.OutboxWriter.Write(obCtx, entry)
			obCancel()
			if writeErr == nil {
				slog.Warn("server: saveMessage direct write failed, enqueued outbox fallback", "session", sessionID, "role", role, "err", lastErr)
				return 0, nil
			}
			slog.Error("server: saveMessage outbox fallback enqueue failed", "session", sessionID, "role", role, "err", writeErr)
		} else {
			slog.Error("server: saveMessage outbox fallback payload marshal failed", "session", sessionID, "role", role, "err", marshalErr)
		}
	}

	return 0, apperr.Wrap(apperr.CodeInternal, "Server.saveMessage", lastErr)
}

// SaveAppView 见 session.Persistence 接口注释（M8f-1）。
func (s *ChatPersistenceService) SaveAppView(ctx context.Context, v session.AppView) error {
	err := s.ChatRepo.SaveAppView(ctx, types.ChatAppViewRow{
		ViewID:      v.ViewID,
		ServerID:    v.ServerID,
		ResourceURI: v.ResourceURI,
		ToolName:    v.ToolName,
		ToolInput:   rawOrEmptyObject(v.ToolInput),
		ToolResult:  rawOrEmptyObject(v.ToolResult),
		WidgetState: "{}",
	})
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "Server.saveAppView", err)
	}
	return nil
}

// rawOrEmptyObject json.RawMessage 为空时落 "{}"（chat_app_views 对应列 NOT NULL）。
func rawOrEmptyObject(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "{}"
	}
	return string(raw)
}

// LinkAppViewsToMessage 见 session.Persistence 接口注释（M8f-1）。SessionID 由
// SaveAppView 写入时已经确定（chat_app_views.session_id 是外部字段，本方法未
// 直接持有，故经参数下发——与 SQLiteChatRepository.LinkAppViewsToMessage 签名一致，
// 避免仓储层再反查一次 view 归属）。
func (s *ChatPersistenceService) LinkAppViewsToMessage(ctx context.Context, sessionID string, viewIDs []string, messageID int64) error {
	if err := s.ChatRepo.LinkAppViewsToMessage(ctx, sessionID, viewIDs, messageID); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "Server.linkAppViewsToMessage", err)
	}
	return nil
}

// ConsumeSessionModelContext 见 session.Persistence 接口注释（M8f-1）。
func (s *ChatPersistenceService) ConsumeSessionModelContext(ctx context.Context, sessionID string) (map[string]json.RawMessage, error) {
	raw, err := s.ChatRepo.ConsumeSessionModelContext(ctx, sessionID)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "Server.consumeSessionModelContext", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		// 列内容理论上恒由本包自身通过 json_set 写入，不应出现非法 JSON；出现时
		// 视为无待注入内容而非阻断整轮推理（防御性降级，记录 Warn 供排查）。
		slog.Warn("server: app_model_context column contains invalid JSON, treating as empty", "session", sessionID, "err", err)
		return nil, nil
	}
	return m, nil
}

func (s *ChatPersistenceService) SampleAndScoreReply(sessionID, query, response string) {
	if s.SamplingMonitor == nil || s.Registry == nil {
		return
	}
	p := s.Registry.PickProvider("default")
	if p == nil {
		p = s.Registry.PickProvider("general")
	}
	s.SamplingMonitor.MaybeSampleAndScore(p, sessionID, query, response)
}

func (s *ChatPersistenceService) UpdateSessionTitle(ctx context.Context, sessionID, firstInput string) error {
	title := firstInput
	if len([]rune(title)) > 40 {
		runes := []rune(title)
		title = string(runes[:40]) + "…"
	}
	err := s.ChatRepo.UpdateSessionTitle(ctx, sessionID, title)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "Server.updateSessionTitle", err)
	}
	return nil
}

func (s *ChatPersistenceService) TouchSession(ctx context.Context, sessionID string) error {
	// [阶段02-错误吞没整改 §2.8] L1（上下文断链）：此前用 context.Background()
	// 而非入参 ctx 派生超时，导致调用方传入的取消信号/deadline/trace 完全丢失
	// （例如上游请求被显式取消后，本次落库仍会跑满 5s 才超时）。改为以 ctx 为
	// 父级派生，保留取消链路，同时仍设 5s 硬超时防止落库卡死拖垮请求处理。
	tctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := s.ChatRepo.TouchSession(tctx, sessionID); err != nil {
		slog.Warn("server: failed to touch session", "session", sessionID, "err", err)
		return apperr.Wrap(apperr.CodeInternal, "ChatPersistenceService.TouchSession", err)
	}
	return nil
}
