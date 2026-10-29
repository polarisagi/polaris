package session

import (
	"context"

	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// AppViewStore MCP Apps 视图落库的最小消费端接口；Persistence 是其超集。
// 聊天回合（orchestrator）与无 LLM 的直接触发（插件设置向导）共用下面两个函数，
// 保证 chat_app_views 的落库字段与"先落视图、后关联消息"的时序只有一份实现。
type AppViewStore interface {
	SaveAppView(ctx context.Context, v AppView) error
	LinkAppViewsToMessage(ctx context.Context, sessionID string, viewIDs []string, messageID int64) error
}

// SaveToolUIView 落库一次工具调用产生的视图快照。session_id 必须随行写入：
// 后续 LinkAppViewsToMessage 与网关的 authorizeMCPAppView 都按它做归属校验。
func SaveToolUIView(ctx context.Context, s AppViewStore, sessionID string, ui *types.ToolUIRef) error {
	err := s.SaveAppView(ctx, AppView{
		SessionID:   sessionID,
		ViewID:      ui.ViewID,
		ServerID:    ui.ServerID,
		ResourceURI: ui.ResourceURI,
		ToolName:    ui.ToolName,
		ToolInput:   ui.ToolInput,
		ToolResult:  ui.ToolResult,
	})
	if err != nil {
		return apperr.Wrap(apperr.CodeOf(err), "session.SaveToolUIView", err)
	}
	return nil
}

// LinkToolUIViews 把视图关联到刚落库的 assistant 消息。messageID<=0（写入降级到 outbox 或失败）
// 或 viewIDs 为空时跳过：都不是错误，只是这一轮没有可关联的东西。
func LinkToolUIViews(ctx context.Context, s AppViewStore, sessionID string, viewIDs []string, messageID int64) error {
	if len(viewIDs) == 0 || messageID <= 0 {
		return nil
	}
	if err := s.LinkAppViewsToMessage(ctx, sessionID, viewIDs, messageID); err != nil {
		return apperr.Wrap(apperr.CodeOf(err), "session.LinkToolUIViews", err)
	}
	return nil
}
