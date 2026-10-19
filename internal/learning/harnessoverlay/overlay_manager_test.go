package harnessoverlay_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/polarisagi/polaris/internal/learning/harnessoverlay"
	"github.com/polarisagi/polaris/pkg/apperr"
)

func TestOverlayManager_LifecycleAndRollback(t *testing.T) {
	tmpDir := t.TempDir()
	overlayDir := filepath.Join(tmpDir, "harness_overlay")

	mgr := harnessoverlay.New(overlayDir)
	ctx := context.Background()

	// 1. Init
	if err := mgr.Init(ctx); err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	// 验证幂等性
	if err := mgr.Init(ctx); err != nil {
		t.Fatalf("Second Init failed: %v", err)
	}

	// 2. ValidatePath 安全边界校验
	validPaths := []string{
		"skills/helper.py",
		"skills/sub/helper.sh",
		"prompt_addenda/01_rule.md",
		"tool_notes/bash.md",
	}
	for _, p := range validPaths {
		if err := mgr.ValidatePath(p); err != nil {
			t.Errorf("expected %q to be valid, got: %v", p, err)
		}
	}

	invalidPaths := []string{
		"../escape.md",
		"/etc/passwd",
		"eval/test.json",
		"configs/core.toml",
		"prompt_addenda/not_md.txt",
		"tool_notes/not_md.json",
		"just_file.md",
	}
	for _, p := range invalidPaths {
		if err := mgr.ValidatePath(p); err == nil {
			t.Errorf("expected %q to be rejected, but succeeded", p)
		}
	}

	// 3. WriteAndCommit Version 1
	v1Files := map[string][]byte{
		"prompt_addenda/02_b.md": []byte("Second instruction block."),
		"prompt_addenda/01_a.md": []byte("First instruction block."),
		"tool_notes/bash.md":     []byte("Run commands with caution and tail logs."),
	}
	h1, err := mgr.WriteAndCommit(ctx, v1Files, "feat: version 1 addenda", "")
	if err != nil {
		t.Fatalf("WriteAndCommit v1 failed: %v", err)
	}
	if len(h1) < 7 {
		t.Fatalf("invalid commit hash: %q", h1)
	}

	// 验证 GetPromptAddenda 字母序拼接
	addenda, err := mgr.GetPromptAddenda(ctx)
	if err != nil {
		t.Fatalf("GetPromptAddenda failed: %v", err)
	}
	expectedAddenda := "First instruction block.\n\nSecond instruction block."
	if addenda != expectedAddenda {
		t.Errorf("unexpected prompt addenda: got %q, want %q", addenda, expectedAddenda)
	}

	// 验证 GetToolNote
	note, err := mgr.GetToolNote(ctx, "bash")
	if err != nil {
		t.Fatalf("GetToolNote failed: %v", err)
	}
	if note != "Run commands with caution and tail logs." {
		t.Errorf("unexpected tool note: %q", note)
	}

	// 4. WriteAndCommit Version 2
	v2Files := map[string][]byte{
		"prompt_addenda/03_c.md": []byte("Third instruction block."),
	}
	h2, err := mgr.WriteAndCommit(ctx, v2Files, "feat: version 2 addenda", "")
	if err != nil {
		t.Fatalf("WriteAndCommit v2 failed: %v", err)
	}

	// 5. History 验证
	history, err := mgr.History(ctx, 10)
	if err != nil {
		t.Fatalf("History failed: %v", err)
	}
	if len(history) < 3 { // initial commit + v1 + v2
		t.Fatalf("expected at least 3 history entries, got %d", len(history))
	}
	if history[0].Hash != h2 {
		t.Errorf("top history hash should be %s, got %s", h2, history[0].Hash)
	}

	// 6. Rollback 到 Version 1
	if err := mgr.Rollback(ctx, h1); err != nil {
		t.Fatalf("Rollback to h1 failed: %v", err)
	}

	// 验证 03_c.md 已被回滚消除
	cFile := filepath.Join(overlayDir, "prompt_addenda", "03_c.md")
	if _, err := os.Stat(cFile); !os.IsNotExist(err) {
		t.Errorf("expected 03_c.md to be deleted after rollback, but still exists")
	}

	addendaAfterRollback, err := mgr.GetPromptAddenda(ctx)
	if err != nil {
		t.Fatalf("GetPromptAddenda after rollback failed: %v", err)
	}
	if addendaAfterRollback != expectedAddenda {
		t.Errorf("unexpected addenda after rollback: got %q, want %q", addendaAfterRollback, expectedAddenda)
	}

	// 验证非法版本字符串被拦截
	if err := mgr.Rollback(ctx, "invalid; rm -rf /"); err == nil {
		t.Errorf("expected invalid rollback version to fail")
	} else if apperr.CodeOf(err) != apperr.CodeInvalidInput {
		t.Errorf("expected CodeInvalidInput, got %v", err)
	}
}

func TestOverlayManager_GetToolNotes(t *testing.T) {
	tmpDir := t.TempDir()
	mgr := harnessoverlay.New(tmpDir)
	ctx := context.Background()
	if err := mgr.Init(ctx); err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	files := map[string][]byte{
		"tool_notes/bash.md": []byte("Note for bash."),
		"tool_notes/grep.md": []byte("Note for grep."),
	}
	if _, err := mgr.WriteAndCommit(ctx, files, "add notes", ""); err != nil {
		t.Fatalf("WriteAndCommit failed: %v", err)
	}

	notes, err := mgr.GetToolNotes(ctx)
	if err != nil {
		t.Fatalf("GetToolNotes failed: %v", err)
	}
	if len(notes) != 2 {
		t.Fatalf("expected 2 notes, got %d", len(notes))
	}
	if notes["bash"] != "Note for bash." || notes["grep"] != "Note for grep." {
		t.Errorf("unexpected notes content: %+v", notes)
	}

	missing, err := mgr.GetToolNote(ctx, "non_existent")
	if err != nil {
		t.Fatalf("GetToolNote missing failed: %v", err)
	}
	if missing != "" {
		t.Errorf("expected empty string for missing note, got %q", missing)
	}
}

// TestV6_HarnessOverlay_ProtectedSurfaces_RejectsEvalConfigAndTrustProfile 验证 harness 覆盖层
// 严格限制可编辑面，拒绝评测配置、回归集与 trust_profile 等敏感路径（V6 / C4）。
func TestV6_HarnessOverlay_ProtectedSurfaces_RejectsEvalConfigAndTrustProfile(t *testing.T) {
	mgr := harnessoverlay.New(t.TempDir())
	ctx := context.Background()
	if err := mgr.Init(ctx); err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	sensitivePaths := []string{
		"eval/test_cases.json",
		"eval/holdout/cases.json",
		"regression/benchmark.json",
		"configs/defaults.toml",
		"configs/trust_profile.toml",
		"trust_profile/policy.cedar",
		"core/agent.go",
	}

	for _, p := range sensitivePaths {
		if err := mgr.ValidatePath(p); err == nil {
			t.Fatalf("V6 缺陷：ValidatePath 必须拒绝敏感面路径 %q", p)
		}
		_, err := mgr.WriteAndCommit(ctx, map[string][]byte{p: []byte("tamper")}, "malicious", "")
		if err == nil {
			t.Fatalf("V6 缺陷：WriteAndCommit 必须拒绝写入敏感面路径 %q", p)
		}
		if apperr.CodeOf(err) != apperr.CodeForbidden {
			t.Errorf("expected CodeForbidden for %q, got: %v", p, err)
		}
	}
}
