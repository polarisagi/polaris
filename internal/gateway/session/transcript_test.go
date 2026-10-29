package session

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTranscript(t *testing.T) {
	tempDir := t.TempDir()

	tw, err := openTranscript(tempDir, "session-1", true)
	if err != nil {
		t.Fatalf("openTranscript failed: %v", err)
	}

	// WriteTurn
	tw.WriteTurn("user", "hello", 100, 10)

	// WriteError
	tw.WriteError("500", "some error")

	// Close
	tw.Close()

	// Check if file exists
	files, err := os.ReadDir(tempDir)
	if err != nil {
		t.Fatalf("ReadDir failed: %v", err)
	}
	if len(files) == 0 {
		t.Errorf("no transcript files created")
	}

	// PruneTranscripts
	PruneTranscripts(tempDir, 0) // 0 days retention

	// File should be deleted
	files, err = os.ReadDir(tempDir)
	if err != nil {
		t.Fatalf("ReadDir failed: %v", err)
	}
	if len(files) > 0 {
		t.Errorf("expected 0 files after prune, got %d", len(files))
	}
}

// TestOpenTranscript_SessionIDValidation_S07 验证 S-07 修复：非法 sessionID
// 一律拒绝且不产生任何文件；合法 sessionID 正常写入且路径落在 dir 内。
// 回归锚点：修复前 filepath.Join(dir, sessionID+".jsonl") 对 "../evil" 之类的
// sessionID 不做任何校验，可在任意目录创建 .jsonl 文件。
func TestOpenTranscript_SessionIDValidation_S07(t *testing.T) {
	longID := ""
	for i := 0; i < 129; i++ {
		longID += "a"
	}

	invalidCases := []string{"../evil", "a/b", "", longID}
	for _, sid := range invalidCases {
		tempDir := t.TempDir()
		tw, err := openTranscript(tempDir, sid, false)
		if err == nil {
			tw.Close()
			t.Errorf("sessionID %q: expected error, got nil", sid)
		}
		entries, rerr := os.ReadDir(tempDir)
		if rerr != nil {
			t.Fatalf("ReadDir failed: %v", rerr)
		}
		if len(entries) != 0 {
			t.Errorf("sessionID %q: expected no files created, got %d", sid, len(entries))
		}
		// "../evil" 还须确认没有文件逃逸到 tempDir 的父目录。
		if sid == "../evil" {
			if _, statErr := os.Stat(filepath.Join(filepath.Dir(tempDir), "evil.jsonl")); statErr == nil {
				t.Error("path traversal succeeded: evil.jsonl created outside tempDir")
			}
		}
	}

	// 合法 sessionID：正常写入，路径落在 dir 内。
	tempDir := t.TempDir()
	sid := "sess-01_A"
	tw, err := openTranscript(tempDir, sid, false)
	if err != nil {
		t.Fatalf("valid sessionID %q: unexpected error: %v", sid, err)
	}
	tw.WriteTurn("user", "hi", 0, 0)
	tw.Close()

	expected := filepath.Join(tempDir, sid+".jsonl")
	if _, statErr := os.Stat(expected); statErr != nil {
		t.Errorf("expected transcript file at %s, stat error: %v", expected, statErr)
	}
}
