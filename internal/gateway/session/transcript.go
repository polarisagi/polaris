package session

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// [A-03 Step2] 本文件自 internal/gateway/server/chat/transcript.go 原样迁入
// （逻辑零改动，仅包名调整）：Transcript 写入是 RunTurn 的编排职责一部分
// （见 orchestrator.go 职责划分注释），且本文件自身零 net/http 依赖，符合
// session 包的硬约束。SessionIDPattern 由此成为 S-07 sessionID 白名单的
// 唯一权威源，chat 包侧（HTTP 边界层"第一层"早期校验）改为引用本变量，
// 不再本地重复定义。

const transcriptVersion = 1

// SessionIDPattern 会话 ID 白名单：仅允许字母数字、短横线、下划线，长度 1~128。
// 用于一切以 sessionID 参与文件路径/表主键构造的场景（S-07，防路径穿越 +
// 防 SQL 侧异常）。导出供 HTTP 边界层（chat/sse.go）做早期校验复用，本文件
// openTranscript 自身仍保留独立的第二层防御性校验（即便调用方遗漏入口
// 校验，这里仍拒绝非法 sessionID）。
var SessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// transcriptEntry 是 JSONL 文件中的一行记录。
// 字段按 type 复用，零值字段 omitempty 不输出，保持文件紧凑。
type transcriptEntry struct {
	Type      string `json:"type"`
	V         int    `json:"v,omitempty"`       // session 行专用
	ID        string `json:"id,omitempty"`      // session 行专用
	Role      string `json:"role,omitempty"`    // turn 行专用
	Content   string `json:"content,omitempty"` // turn 行专用
	Code      string `json:"code,omitempty"`    // error 行专用
	Msg       string `json:"msg,omitempty"`     // error 行专用
	TS        string `json:"ts"`
	LatencyMs int64  `json:"latency_ms,omitempty"` // assistant turn 专用
	Tokens    int    `json:"tokens,omitempty"`     // assistant turn 专用
}

// TranscriptWriter 以追加模式写 per-session JSONL transcript 文件。
// 单 goroutine 使用，无需额外加锁（每个请求独享一个实例）。
type TranscriptWriter struct {
	f *os.File
}

// openTranscript 打开（或创建）sessionID 对应的 transcript 文件。
// writeHeader=true 时追加会话起始行（isFirstTurn 时使用）。
func openTranscript(dir, sessionID string, writeHeader bool) (*TranscriptWriter, error) {
	// S-07 落盘处兜底校验（双重防御第二层，第一层见 chat/sse.go 入口白名单）：
	// 即便调用方遗漏入口校验，这里仍拒绝非法 sessionID，且做路径归一化前缀
	// 断言防御符号链接/../ 变体。
	if !SessionIDPattern.MatchString(sessionID) {
		return nil, apperr.New(apperr.CodeInvalidInput, "openTranscript: invalid session id")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "openTranscript", err)
	}
	cleanDir := filepath.Clean(dir) + string(os.PathSeparator)
	path := filepath.Clean(filepath.Join(dir, sessionID+".jsonl"))
	if !strings.HasPrefix(path, cleanDir) {
		return nil, apperr.New(apperr.CodeForbidden, "openTranscript: path escapes transcript dir")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "openTranscript", err)
	}
	tw := &TranscriptWriter{f: f}
	if writeHeader {
		tw.write(transcriptEntry{Type: "session", V: transcriptVersion, ID: sessionID, TS: tsNow()})
	}
	return tw, nil
}

// WriteTurn 追加一条对话轮次（user 或 assistant）。
// latencyMs / tokens 仅在非零时写出（assistant turn 专用）。
func (tw *TranscriptWriter) WriteTurn(role, content string, latencyMs int64, tokens int) {
	e := transcriptEntry{Type: "turn", Role: role, Content: content, TS: tsNow()}
	if latencyMs > 0 {
		e.LatencyMs = latencyMs
	}
	if tokens > 0 {
		e.Tokens = tokens
	}
	tw.write(e)
}

// WriteError 追加一条错误事件。
func (tw *TranscriptWriter) WriteError(code, msg string) {
	tw.write(transcriptEntry{Type: "error", Code: code, Msg: msg, TS: tsNow()})
}

// Close 关闭文件句柄。defer 调用，幂等。
func (tw *TranscriptWriter) Close() {
	if tw.f != nil {
		tw.f.Close()
		tw.f = nil
	}
}

func (tw *TranscriptWriter) write(e transcriptEntry) {
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	b = append(b, '\n')
	_, _ = tw.f.Write(b)
}

func tsNow() string {
	return time.Now().Format(time.RFC3339)
}

// PruneTranscripts 删除超过 retentionDays 天未修改的 .jsonl transcript 文件。
// 在服务启动时以 goroutine 调用，非阻塞，目录不存在时静默返回。
func PruneTranscripts(dir string, retentionDays int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().AddDate(0, 0, -retentionDays)
	pruned := 0
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".jsonl" {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(dir, e.Name()))
			pruned++
		}
	}
	if pruned > 0 {
		slog.Info("transcript: pruned old files", "count", pruned, "retention_days", retentionDays)
	}
}
