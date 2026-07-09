package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestWasmtime_NullByteOutput(t *testing.T) {
	// 动态编译一个输出带有 \0 的 wasm 模块
	src := `package main

import (
	"fmt"
)

func main() {
	fmt.Print("hello\x00world")
}
`
	tmpDir := t.TempDir()
	srcFile := filepath.Join(tmpDir, "main.go")
	if err := os.WriteFile(srcFile, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}

	wasmFile := filepath.Join(tmpDir, "main.wasm")
	cmd := exec.Command("go", "build", "-o", wasmFile, "main.go")
	cmd.Dir = tmpDir
	cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("Failed to compile wasm (maybe go waisp1 is not configured): %v\nOutput: %s", err, string(out))
	}

	wasmBytes, err := os.ReadFile(wasmFile)
	if err != nil {
		t.Fatal(err)
	}

	// 必须先初始化 engine
	if err := WasmtimeInit(); err != nil {
		t.Fatalf("WasmtimeInit failed: %v", err)
	}

	outJSON, err := WasmtimeExecute(wasmBytes, "{}", "", 256, false, 100_000_000, 0)
	if err != nil {
		t.Fatalf("WasmtimeExecute failed: %v", err)
	}

	if outJSON != "hello\x00world" {
		t.Errorf("Expected 'hello\\x00world', got %q", outJSON)
	}
}
