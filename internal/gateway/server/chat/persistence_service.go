package chat

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/polarisagi/polaris/internal/eval/analysis"
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
	err := s.ChatRepo.CreateSession(ctx, types.ChatSessionRow{ID: sessionID, Title: ""})
	if err != nil {
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

func (s *ChatPersistenceService) SaveMessage(ctx context.Context, sessionID, role, content string, toolCalls string, reasoningContent string, durationMs int64) error {
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
		if err := s.ChatRepo.AppendMessage(ctx, row); err != nil {
			lastErr = err
			slog.Warn("server: saveMessage attempt failed, will retry", "session", sessionID, "role", role, "attempt", attempt+1, "err", err)
			continue
		}
		return nil
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
				return nil
			}
			slog.Error("server: saveMessage outbox fallback enqueue failed", "session", sessionID, "role", role, "err", writeErr)
		} else {
			slog.Error("server: saveMessage outbox fallback payload marshal failed", "session", sessionID, "role", role, "err", marshalErr)
		}
	}

	return apperr.Wrap(apperr.CodeInternal, "Server.saveMessage", lastErr)
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
