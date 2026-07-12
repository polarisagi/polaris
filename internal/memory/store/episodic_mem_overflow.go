package store

import (
	"fmt"
	"path/filepath"
)

// R7 拆分（2026-07-12）：GR-5-001 修复（VFS 落盘替换 os.WriteFile 直写）新增的
// BlobOverflowWriter 消费端接口 + truncateEpisodicPayload 落盘逻辑从 episodic_mem.go
// 抽出至本文件，使主文件回落到 400 行上限内；行为与拆分前逐行等价。

// BlobOverflowWriter 消费端窄接口（R1.4）：episodic 层超限 Payload 落盘所需的
// 最小写入能力，由 *vfs.WorkspaceManager 结构化满足（Go 隐式接口，无需
// vfs 包反向依赖 memory/store）。nil 时降级为进程内截断（不落盘完整内容，
// 仅保留 512 字节预览），不再直接调用 os.MkdirAll/os.WriteFile（HE-6）。
type BlobOverflowWriter interface {
	WriteFile(relPath string, data []byte) error
}

// SetBlobOverflowWriter 注入超限 Payload 落盘目标（通常为 *vfs.WorkspaceManager）。
// 未注入时 truncateEpisodicPayload 不落盘完整内容，仅保留截断预览（GR-5-001 修复）。
func (em *EpisodicMem) SetBlobOverflowWriter(w BlobOverflowWriter) {
	em.vfsWriter = w
}

// truncateEpisodicPayload 将超限 Payload 落盘，返回含 log_ref 占位符的截断版本。
// 落盘路径：workspace_vfs 相对路径 logs/events/<id>.bin（经 em.vfsWriter 写入，
// 通常为 *vfs.WorkspaceManager，落在其 rootDir 隔离边界内）。
// 返回内容：前 512 字节（BM25 可用）+ log_ref JSON 片段。
//
// GR-5-001 修复：原实现直接调用 os.MkdirAll/os.WriteFile 并硬编码
// ~/.polarisagi/polaris/logs/events/ 绝对路径，绕过 VFS 隔离边界（HE-6：
// "单行载荷超 4KB 必须卸载至 VFS，禁止在 memory 层直接调用 os.WriteFile"）。
// 未注入 vfsWriter 时（如未接入 VFS 的最小化 Tier-0 部署/单测），降级为
// 仅保留截断预览、不落盘完整内容——不再绕过分层直接写宿主文件系统。
func (em *EpisodicMem) truncateEpisodicPayload(eventID string, raw []byte) []byte {
	logRef := eventID // 无 vfsWriter 时 log_ref 仅作标识，不指向任何实际落盘文件
	if em.vfsWriter != nil {
		relPath := filepath.Join("logs", "events", eventID+".bin")
		if err := em.vfsWriter.WriteFile(relPath, raw); err == nil {
			logRef = relPath
		}
	}

	preview := raw
	if len(preview) > 512 {
		preview = preview[:512]
	}
	ref := fmt.Sprintf(
		`{"log_ref":%q,"bytes":%d,"preview":%s}`,
		logRef, len(raw), string(preview),
	)
	return []byte(ref)
}
