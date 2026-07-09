package str_replace_editor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/tool/builtin/guard"
	"github.com/polarisagi/polaris/pkg/apperr"
)

type strReplaceEditorArgs struct {
	Command string `json:"command"` // create, str_replace, view, undo_edit
	Path    string `json:"path"`
	OldStr  string `json:"old_str"`
	NewStr  string `json:"new_str"`
}

func MakeStrReplaceEditorFn(allowedPaths []string) sandbox.InProcessFn {
	// undoBuffer 保存最近一次 str_replace_editor 修改的文件备份（undo_edit 恢复用）。
	// DAGExecutor 并发执行节点时多个 goroutine 可能同时调用 str_replace_editor，必须加锁保护。
	undoBuffer := make(map[string]string)
	var undoBufferMu sync.Mutex
	return func(ctx context.Context, input []byte) ([]byte, error) {
		var args strReplaceEditorArgs
		if err := json.Unmarshal(input, &args); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "str_replace_editor: invalid args", err)
		}
		if err := guard.CheckAllowedPath(args.Path, allowedPaths); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "makeStrReplaceEditorFn", err)
		}

		cleanPath := filepath.Clean(args.Path)

		switch args.Command {
		case "create":
			if _, err := os.Stat(cleanPath); err == nil {
				return nil, apperr.New(apperr.CodeInternal, "str_replace_editor: file already exists")
			}
			if err := os.WriteFile(cleanPath, []byte(args.NewStr), 0600); err != nil {
				return nil, apperr.Wrap(apperr.CodeInternal, "str_replace_editor: create failed", err)
			}
			return []byte(`{"status":"created"}`), nil

		case "view":
			data, err := os.ReadFile(cleanPath)
			if err != nil {
				return nil, apperr.Wrap(apperr.CodeInternal, "str_replace_editor: view failed", err)
			}
			return data, nil

		case "str_replace":
			return executeStrReplace(cleanPath, args, undoBuffer, &undoBufferMu)

		case "undo_edit":
			undoBufferMu.Lock()
			oldContent, ok := undoBuffer[cleanPath]
			if ok {
				delete(undoBuffer, cleanPath)
			}
			undoBufferMu.Unlock()
			if !ok {
				return nil, apperr.New(apperr.CodeInternal, "str_replace_editor: no undo history found for this file")
			}
			if err := os.WriteFile(cleanPath, []byte(oldContent), 0600); err != nil {
				return nil, apperr.Wrap(apperr.CodeInternal, "str_replace_editor: undo write failed", err)
			}
			return []byte(`{"status":"undone"}`), nil

		default:
			return nil, apperr.New(apperr.CodeInternal, fmt.Sprintf("str_replace_editor: unknown command %q", args.Command))
		}
	}
}

func executeStrReplace(cleanPath string, args strReplaceEditorArgs, undoBuffer map[string]string, mu *sync.Mutex) ([]byte, error) {
	data, err := os.ReadFile(cleanPath)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "str_replace_editor: read failed", err)
	}
	content := string(data)

	if args.OldStr == "" {
		return nil, apperr.New(apperr.CodeInternal, "str_replace_editor: old_str cannot be empty")
	}

	count := strings.Count(content, args.OldStr)
	if count == 0 {
		return nil, apperr.New(apperr.CodeInternal, "str_replace_editor: old_str not found in file")
	}
	if count > 1 {
		return nil, apperr.New(apperr.CodeInternal, "str_replace_editor: old_str is not unique, matched multiple times. Please provide more context in old_str.")
	}

	// 备份到 undoBuffer（加锁：多个节点并发执行 str_replace_editor 时防竞争）
	mu.Lock()
	undoBuffer[cleanPath] = content
	mu.Unlock()

	newContent := strings.Replace(content, args.OldStr, args.NewStr, 1)
	if err := os.WriteFile(cleanPath, []byte(newContent), 0600); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "str_replace_editor: write failed", err)
	}
	return []byte(`{"status":"replaced"}`), nil
}
