package read_tool_ref

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"unicode/utf8"

	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/pkg/apperr"
)

type readToolRefArgs struct {
	TaskID string `json:"task_id"`
	ID     string `json:"id"`
	// Offset/Limit 按字节分窗读取。内核对单条工具结果注入有 8000 字节上限（超限取首尾、丢中段），
	// 若本工具一次返回全文，中段永远无法取回且每次重读都会重新触发截断。
	Offset int `json:"offset"`
	Limit  int `json:"limit"`
}

// defaultWindowBytes 默认窗口，低于内核注入上限（agent.maxExecResultBytes=8000）并为头部提示留余量，
// 保证单次返回不会再被首尾截断。
const defaultWindowBytes = 6000

func MakeReadToolRefFn(vfsRoot string) sandbox.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		var args readToolRefArgs
		if err := json.Unmarshal(input, &args); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "read_tool_ref: invalid args", err)
		}
		if args.TaskID == "" || args.ID == "" {
			return nil, apperr.New(apperr.CodeInternal, "read_tool_ref: task_id and id are required")
		}

		if vfsRoot == "" {
			return nil, apperr.New(apperr.CodeInternal, "read_tool_ref: vfsRoot not configured")
		}

		// Security: prevent path traversal
		cleanTaskID := filepath.Clean(args.TaskID)
		cleanID := filepath.Base(args.ID)

		// Ensure cleanTaskID doesn't escape vfsRoot
		if filepath.IsAbs(cleanTaskID) || cleanTaskID == ".." || len(cleanTaskID) > 2 && cleanTaskID[:3] == "../" {
			return nil, apperr.New(apperr.CodeForbidden, "read_tool_ref: invalid task_id path")
		}

		path := filepath.Join(vfsRoot, cleanTaskID, "tool_refs", cleanID+".log")

		data, err := os.ReadFile(path)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "read_tool_ref: file read error", err)
		}
		return window(data, args.Offset, args.Limit), nil
	}
}

// window 返回 data[offset:offset+limit]。全文不超过窗口且未指定 offset 时原样返回（保持旧行为）；
// 否则首行声明区间与总长，模型据 next 继续读取。切点回退到 UTF-8 字符边界。
func window(data []byte, offset, limit int) []byte {
	if limit <= 0 {
		limit = defaultWindowBytes
	}
	if offset < 0 {
		offset = 0
	}
	if offset == 0 && len(data) <= limit {
		return data
	}
	if offset >= len(data) {
		return []byte(fmt.Sprintf("[offset %d beyond end: %d bytes total]", offset, len(data)))
	}
	start := offset
	for start < len(data) && !utf8.RuneStart(data[start]) {
		start++
	}
	end := min(start+limit, len(data))
	for end < len(data) && end > start && !utf8.RuneStart(data[end]) {
		end--
	}
	head := fmt.Sprintf("[bytes %d-%d of %d]", start, end, len(data))
	if end < len(data) {
		head += fmt.Sprintf(" next offset=%d", end)
	}
	return append([]byte(head+"\n"), data[start:end]...)
}
