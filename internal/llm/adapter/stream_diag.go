package adapter

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"
)

// EnvLLMRawDump 开启原始流转储的环境变量，值为落盘目录；未设置（默认）即关闭。
// 转储内容含完整对话（请求摘要只含消息数与工具名，但 SSE 行含模型输出与思考链），
// 仅用于排查"空输出/工具调用丢失"这类需要看原始帧的问题，用完请关闭并删除目录。
const EnvLLMRawDump = "POLARIS_LLM_RAW_DUMP"

// rawDumpDir 每次流式请求读取一次环境变量（廉价），避免包级可变状态；
// 取值为空即关闭。
func rawDumpDir() string { return os.Getenv(EnvLLMRawDump) }

// EnvLLMRawDumpFull 为 "1" 时额外落盘完整请求体（与 .sse 同名、后缀 .req.json），
// 供 tools/cache_diff.go 比较相邻两次请求定位前缀缓存失配（ADR-0120 决策八）。
// 请求体不含鉴权头，但含完整对话：只在本机排障时开启，用完删除目录。
const EnvLLMRawDumpFull = "POLARIS_LLM_RAW_DUMP_FULL"

// rawDump 单条流的转储句柄；nil 表示未开启，所有方法对 nil 安全。
type rawDump struct {
	mu sync.Mutex
	f  *os.File
}

// newRawDump 开启时创建 <dir>/<unixnano>-<随机>.sse 并写入请求摘要。
// 只写模型名、消息数、工具名与关键开关——绝不写 API Key、鉴权头或消息正文。
func newRawDump(dir string, req *OpenAIRequest) *rawDump {
	if dir == "" || req == nil {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		slog.Warn("llm raw dump: mkdir failed; dump disabled", "err", err)
		return nil
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() {
		slog.Warn("llm raw dump: diagnostic path is not a directory; dump disabled", "err", err)
		return nil
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		slog.Warn("llm raw dump: secure directory failed; dump disabled", "err", err)
		return nil
	}
	// CreateTemp 保证文件名唯一（并发流不会撞名），且权限固定 0600。
	f, err := os.CreateTemp(dir, fmt.Sprintf("%d-*.sse", time.Now().UnixNano()))
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
	if os.Getenv(EnvLLMRawDumpFull) == "1" {
		writeFullRequest(strings.TrimSuffix(f.Name(), ".sse")+".req.json", req)
	}
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
	if d.f == nil {
		return
	}
	if _, err := d.f.WriteString(line + "\n"); err != nil {
		// 转储只是调试旁路：写失败（磁盘满等）不影响推理，但要留一条告警并停止继续写，避免刷屏。
		slog.Warn("llm raw dump: write failed, dump disabled for this stream", "err", err)
		_ = d.f.Close() // 旁路文件，关闭失败无可补救
		d.f = nil
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

// writeFullRequest 以 0600 写完整请求体；失败只告警，不影响推理。
func writeFullRequest(path string, req *OpenAIRequest) {
	body, err := json.MarshalIndent(req, "", " ")
	if err != nil {
		slog.Warn("llm raw dump: marshal full request failed", "err", err)
		return
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		slog.Warn("llm raw dump: write full request failed", "path", path, "err", err)
	}
}
