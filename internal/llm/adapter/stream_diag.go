package adapter

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// EnvLLMRawDump 开启原始流转储的环境变量，值为落盘目录；未设置（默认）即关闭。
// 转储内容含完整对话（请求摘要只含消息数与工具名，但 SSE 行含模型输出与思考链），
// 仅用于排查"空输出/工具调用丢失"这类需要看原始帧的问题，用完请关闭并删除目录。
const EnvLLMRawDump = "POLARIS_LLM_RAW_DUMP"

var (
	rawDumpSeq atomic.Int64
	// 进程内只读一次：转储开关属于启动期调试设置，运行中改环境变量本就不会生效。
	rawDumpDirOnce sync.Once
	rawDumpDirVal  string
)

func rawDumpDir() string {
	rawDumpDirOnce.Do(func() { rawDumpDirVal = os.Getenv(EnvLLMRawDump) })
	return rawDumpDirVal
}

// rawDump 单条流的转储句柄；nil 表示未开启，所有方法对 nil 安全。
type rawDump struct {
	mu sync.Mutex
	f  *os.File
}

// newRawDump 开启时创建 <dir>/<unixnano>-<seq>.sse 并写入请求摘要。
// 只写模型名、消息数、工具名与关键开关——绝不写 API Key、鉴权头或消息正文。
func newRawDump(dir string, req *OpenAIRequest) *rawDump {
	if dir == "" || req == nil {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		slog.Warn("llm raw dump: mkdir failed", "dir", dir, "err", err)
		return nil
	}
	name := fmt.Sprintf("%d-%d.sse", time.Now().UnixNano(), rawDumpSeq.Add(1))
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		slog.Warn("llm raw dump: open failed", "dir", dir, "err", err)
		return nil
	}
	tools := make([]string, 0, len(req.Tools))
	for _, t := range req.Tools {
		tools = append(tools, t.Function.Name)
	}
	thinking := ""
	if req.Thinking != nil {
		thinking = req.Thinking.Type
	}
	summary, _ := json.Marshal(map[string]any{
		"time":             time.Now().Format(time.RFC3339Nano),
		"model":            req.Model,
		"messages":         len(req.Messages),
		"tools":            tools,
		"tool_choice":      req.ToolChoice,
		"thinking":         thinking,
		"reasoning_effort": req.ReasoningEffort,
	})
	d := &rawDump{f: f}
	d.write("# request " + string(summary))
	return d
}

// Line 追加一条原始 SSE 行。
func (d *rawDump) Line(line string) {
	if d == nil {
		return
	}
	d.write(line)
}

func (d *rawDump) write(line string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.f != nil {
		_, _ = d.f.WriteString(line + "\n")
	}
}

// Close 关闭转储文件。
func (d *rawDump) Close() {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.f != nil {
		_ = d.f.Close()
		d.f = nil
	}
}
