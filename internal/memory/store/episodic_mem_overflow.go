package store

import (
	"encoding/json"
	"log/slog"
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

// episodicOverflowRef 是 truncateEpisodicPayload 的落盘引用结构（ADR-0094 决策六：
// 结构化载体禁字符串直拼）。json.Marshal 保证 Preview 中出现的引号/换行/控制字符
// 被正确转义，不会像原先的 fmt.Sprintf(`"preview":%s`) 那样直接拼出语法损坏的 JSON。
type episodicOverflowRef struct {
	LogRef  string `json:"log_ref"`
	Bytes   int    `json:"bytes"`
	Preview string `json:"preview"`
}

// safeTruncateUTF8Bytes 按字节预算截断字符串，但不切断多字节 UTF-8 字符——
// 512 字节的硬截断此前直接对 raw []byte 做 preview[:512]，若截断点落在一个
// 多字节 rune 中间，preview 就会含有非法 UTF-8 字节序列，json.Marshal 时会被
// 替换为 U+FFFD 乱码。改为按 rune 边界回退到不超过 maxBytes 的最近安全位置。
func safeTruncateUTF8Bytes(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && !utf8ValidStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// utf8ValidStart 判断 b 是否是一个 UTF-8 字符序列的起始字节（ASCII 或多字节序列
// 的首字节），而不是某个多字节字符中间的延续字节（10xxxxxx）。
func utf8ValidStart(b byte) bool {
	return b&0xC0 != 0x80
}

// truncateEpisodicPayload 将超限 Payload 落盘，返回含 log_ref 占位符的截断版本。
// 落盘路径：workspace_vfs 相对路径 logs/events/<id>.bin（经 em.vfsWriter 写入，
// 通常为 *vfs.WorkspaceManager，落在其 rootDir 隔离边界内）。
// 返回内容：前 512 字节（BM25 可用，按 UTF-8 边界截断）+ log_ref JSON。
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

	preview := safeTruncateUTF8Bytes(string(raw), 512)
	out, err := json.Marshal(episodicOverflowRef{
		LogRef:  logRef,
		Bytes:   len(raw),
		Preview: preview,
	})
	if err != nil {
		// json.Marshal 对 string 字段实质不会失败（除非遇到无法表达的类型，这里
		// 全是基础类型），留一条日志而非静默吞掉，防止未来字段类型变更引入此路径。
		slog.Warn("episodic_mem_overflow: marshal overflow ref failed, falling back to log_ref only",
			"event_id", eventID, "err", err)
		out, _ = json.Marshal(episodicOverflowRef{LogRef: logRef, Bytes: len(raw)})
	}
	return out
}
