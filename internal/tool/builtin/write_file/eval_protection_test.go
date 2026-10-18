package write_file_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/tool/builtin/write_file"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// TestWriteFile_ProtectedEvalConfig 验证评测器配置与评测用例对 Agent 工具不可写。
// 当 Agent 工具试图通过 write_file 改写评测器配置或覆盖层提示词时，被 path_guard 强制阻断（返回 CodeForbidden）。
func TestWriteFile_ProtectedEvalConfig(t *testing.T) {
	layout, err := config.DefaultDataLayout()
	if err != nil {
		t.Fatalf("DefaultDataLayout failed: %v", err)
	}

	fn := write_file.MakeWriteFileFn([]string{"/tmp", layout.Root})

	testCases := []struct {
		name string
		path string
	}{
		{
			name: "Eval root directory",
			path: filepath.Join(layout.Eval, "test_cases.json"),
		},
		{
			name: "Eval holdout partition",
			path: filepath.Join(layout.EvalHoldout, "cases.json"),
		},
		{
			name: "Config directory",
			path: filepath.Join(layout.Config, "eval_runner.toml"),
		},
		{
			name: "Harness overlay prompt addenda (direct modification forbidden)",
			path: filepath.Join(layout.HarnessOverlay, "prompt_addenda", "evil.md"),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			input, err := json.Marshal(map[string]any{
				"path":    tc.path,
				"content": "malicious overwrite",
			})
			if err != nil {
				t.Fatalf("marshal error: %v", err)
			}

			_, err = fn(context.Background(), input)
			if err == nil {
				t.Fatalf("expected write to %q to be rejected, but succeeded", tc.path)
			}

			if apperr.CodeOf(err) != apperr.CodeForbidden {
				t.Fatalf("expected CodeForbidden, got %v (code: %v)", err, apperr.CodeOf(err))
			}
			if !strings.Contains(err.Error(), "forbidden directory") {
				t.Errorf("expected forbidden directory error message, got %v", err)
			}
		})
	}
}
