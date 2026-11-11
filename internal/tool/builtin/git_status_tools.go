package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/tool/builtin/bash"
	"github.com/polarisagi/polaris/internal/tool/builtin/guard"
	"github.com/polarisagi/polaris/pkg/apperr"
)

type gitStatusFile struct {
	Path           string `json:"path"`
	IndexStatus    string `json:"index_status"`
	WorktreeStatus string `json:"worktree_status"`
	OriginalPath   string `json:"original_path,omitempty"`
}

// MakeGitStatusFn 保持 Git porcelain 的 NUL 边界，避免路径内容被误拆成记录。
func MakeGitStatusFn(allowedPaths []string, sandboxEnabled bool, bwrapPath string) sandbox.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		var args struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(input, &args); err != nil {
			return nil, apperr.Wrap(apperr.CodeInvalidInput, "git_status: invalid args", err)
		}
		if args.Path == "" {
			return nil, apperr.New(apperr.CodeInvalidInput, "git_status: path is required")
		}
		if err := guard.CheckReadablePath(ctx, args.Path, guard.ScopedPaths(ctx, allowedPaths)); err != nil {
			return nil, apperr.Wrap(apperr.CodeForbidden, "git_status: path is not readable", err)
		}
		workDir := filepath.Clean(args.Path)
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		gitArgs := []string{"-c", "core.fsmonitor=false", "--no-optional-locks", "status", "--porcelain=v1", "-z", "--branch", "--untracked-files=normal"}
		out, err := bash.RunSandboxedArgv(ctx, protocol.CallerBuiltin, "git", gitArgs, workDir, []string{workDir}, false, 30000, sandboxEnabled, bwrapPath)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInvalidInput,
				fmt.Sprintf("git_status: git status failed: %s", strings.TrimSpace(string(out))), err)
		}
		branch, files, err := parseGitStatusPorcelain(out)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "git_status: invalid porcelain output", err)
		}
		result, err := json.Marshal(map[string]any{"branch": branch, "files": files, "clean": len(files) == 0})
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "git_status: encode result", err)
		}
		return result, nil
	}
}

func parseGitStatusPorcelain(data []byte) (string, []gitStatusFile, error) {
	records := bytesSplitNUL(data)
	branch := ""
	files := make([]gitStatusFile, 0)
	for i := 0; i < len(records); i++ {
		record := records[i]
		if strings.HasPrefix(record, "## ") {
			branch = parseGitStatusBranch(strings.TrimPrefix(record, "## "))
			continue
		}
		if len(record) < 4 || record[2] != ' ' {
			return "", nil, apperr.New(apperr.CodeInternal, "malformed status record")
		}
		file := gitStatusFile{
			IndexStatus:    string(record[0]),
			WorktreeStatus: string(record[1]),
			Path:           record[3:],
		}
		if record[0] == 'R' || record[0] == 'C' || record[1] == 'R' || record[1] == 'C' {
			i++
			if i >= len(records) {
				return "", nil, apperr.New(apperr.CodeInternal, "rename record is missing original path")
			}
			file.OriginalPath = records[i]
		}
		files = append(files, file)
	}
	return branch, files, nil
}

func parseGitStatusBranch(header string) string {
	if strings.HasPrefix(header, "No commits yet on ") {
		return strings.TrimPrefix(header, "No commits yet on ")
	}
	if header == "HEAD (no branch)" {
		return "HEAD"
	}
	branch := header
	if i := strings.Index(branch, "..."); i >= 0 {
		branch = branch[:i]
	} else if i := strings.Index(branch, " ["); i >= 0 {
		branch = branch[:i]
	}
	return branch
}

func bytesSplitNUL(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	parts := strings.Split(string(data), "\x00")
	if parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts
}
