package chat

import (
	"context"

	"github.com/polarisagi/polaris/pkg/apperr"

	apptypes "github.com/polarisagi/polaris/pkg/types"
)

// splitMessages/calcSummaryBudget/summarize/buildTranscript/injectTaskCanvas/
// offloadLargeToolResults 算法本体 2026-07-22 迁移至 internal/memory/compact
// （M4/M5 共享压缩算法，见该包 doc 注释）。本文件只保留网关专属的
// persistCompacted（chat_messages 持久化回写，R7 拆分自 compressor.go；
// Compressor 核心结构与 Compact/ForceCompact/compact 编排逻辑见 compressor.go）。

// persistCompacted 原子替换 chat_messages：删除旧消息，写入摘要 + tail。
// 在事务内完成，保证 SQLite 单连接安全。
func (c *Compressor) persistCompacted(ctx context.Context, sessionID string, summary apptypes.Message, tail []apptypes.Message) error {
	msgs := make([]apptypes.ChatMessageRow, 0, len(tail)+1)
	msgs = append(msgs, apptypes.ChatMessageRow{Role: summary.Role, Content: summary.Content})
	for _, m := range tail {
		msgs = append(msgs, apptypes.ChatMessageRow{Role: m.Role, Content: m.Content})
	}
	if err := c.chatRepo.ReplaceSessionMessages(ctx, sessionID, msgs); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "Compressor.persistCompacted", err)
	}
	return nil
}

// injectTaskCanvas/toolOffloadThreshold/offloadLargeToolResults 已迁移至
// internal/memory/compact.InjectTaskCanvas/ToolOffloadThreshold/
// OffloadLargeToolResults（M4/M5 共享，见该包 doc 注释）。
//
// 2026-07-22 一致性审查订正：此前紧邻此处的注释仍写着"工具输出预裁剪暂未在
// chat 压缩路径实现"，但下方 offloadLargeToolResults 函数当时其实已经
// 完整实现并通过 compressor.go 的 SetToolRefOffloader/boot_server.go 真实
// 接入 internal/memory.ToolRefOffloader（VFS 落盘 + workspace_vfs 登记）——
// 该注释描述的"三处致命缺陷"版本早已被替换，只是文字没有同步更新，属于
// 已知的注释漂移模式（见 memory 记录 polaris-comment-drift-bug）。
