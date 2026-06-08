package server

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

const transcriptVersion = 1

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
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, sessionID+".jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
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
