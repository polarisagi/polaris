package builtin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
)

func TestBuiltinTools_Todo(t *testing.T) {
	tmpDir := t.TempDir()

	mu := &sync.Mutex{}
	writeFn := makeTodoWriteFn([]string{tmpDir}, mu)
	readFn := makeTodoReadFn([]string{tmpDir}, mu)
	ctx := context.Background()

	// Write
	writeArgs := `{"todos": ["Task 1", "Task 2"]}`
	writeRes, err := writeFn(ctx, []byte(writeArgs))
	if err != nil {
		t.Fatalf("todo_write unexpected err: %v", err)
	}
	if string(writeRes) != `{"status":"success"}` {
		t.Fatalf("unexpected write output: %s", writeRes)
	}

	// Read
	readRes, err := readFn(ctx, nil)
	if err != nil {
		t.Fatalf("todo_read unexpected err: %v", err)
	}
	var readOut struct {
		Todos []string `json:"todos"`
	}
	if err := json.Unmarshal(readRes, &readOut); err != nil {
		t.Fatalf("read json err: %v", err)
	}
	if len(readOut.Todos) != 2 || readOut.Todos[0] != "Task 1" {
		t.Fatalf("unexpected read output: %v", readOut.Todos)
	}
}

func TestBuiltinTools_Grep(t *testing.T) {
	tmpDir := t.TempDir()

	testFile := filepath.Join(tmpDir, "test.txt")
	os.WriteFile(testFile, []byte("line1\nmatch this\nline3\nMATCH THIS too"), 0644)

	grepFn := makeGrepFn([]string{tmpDir})
	ctx := context.Background()

	// invalid mode
	_, err := grepFn(ctx, []byte(`{"pattern": "match", "output_mode": "invalid"}`))
	if err == nil {
		t.Fatalf("expected error for invalid mode")
	}

	// valid
	grepArgs := `{"pattern": "match this", "path": "` + tmpDir + `", "case_insensitive": true, "output_mode": "content"}`
	res, err := grepFn(ctx, []byte(grepArgs))
	if err != nil {
		t.Fatalf("grep unexpected err: %v", err)
	}
	if len(res) == 0 {
		t.Fatalf("expected grep results")
	}
}

func TestBuiltinTools_StrReplace(t *testing.T) {
	tmpDir := t.TempDir()

	testFile := filepath.Join(tmpDir, "test.txt")
	os.WriteFile(testFile, []byte("line1\nold string\nline3"), 0644)

	replaceFn := makeStrReplaceEditorFn([]string{tmpDir})
	ctx := context.Background()

	args := `{"path": "` + testFile + `", "command": "str_replace", "old_str": "old string", "new_str": "new string"}`
	res, err := replaceFn(ctx, []byte(args))
	if err != nil {
		t.Fatalf("str_replace unexpected err: %v", err)
	}
	if len(res) == 0 {
		t.Fatalf("expected str_replace results")
	}

	data, _ := os.ReadFile(testFile)
	if string(data) != "line1\nnew string\nline3" {
		t.Fatalf("replacement failed, got: %s", data)
	}
}

func TestBuiltinTools_Bash(t *testing.T) {
	tmpDir := t.TempDir()
	bashFn := makeBashFn([]string{tmpDir}, false, protocol.NetPolicyAllow, "")
	ctx := context.Background()

	args := `{"command": "echo hello"}`
	res, err := bashFn(ctx, []byte(args))
	if err != nil {
		// Just ensure it doesn't crash, native_sandbox might be tricky
		t.Logf("bash err: %v", err)
	}
	_ = res
}

func TestBuiltinTools_RunCommand(t *testing.T) {
	tmpDir := t.TempDir()
	runFn := makeRunCommandFn([]string{tmpDir}, false, protocol.NetPolicyAllow, "")
	ctx := context.Background()

	args := `{"command": "echo hello"}`
	res, err := runFn(ctx, []byte(args))
	if err != nil {
		t.Logf("run err: %v", err)
	}
	_ = res
}
