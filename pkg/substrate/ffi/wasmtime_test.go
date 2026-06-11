package ffi_test

import (
	"context"
	"testing"

	"github.com/polarisagi/polaris/pkg/substrate/ffi"
)

func TestWasmtimePing(t *testing.T) {
	engine := ffi.NewWasmtimeEngine()
	if err := engine.Init(); err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	res := engine.Ping()
	if res != 42 {
		t.Fatalf("Ping expected 42, got %d", res)
	}
}

func TestWasmtimeExecute(t *testing.T) {
	engine := ffi.NewWasmtimeEngine()
	if err := engine.Init(); err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	// minimal valid wasm module (empty)
	wasmBytes := []byte{0x00, 0x61, 0x73, 0x6D, 0x01, 0x00, 0x00, 0x00}
	out, err := engine.Execute(context.TODO(), wasmBytes, ffi.ExecuteOptions{
		Input: `{"test": "input"}`,
	})
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if out != "" {
		t.Fatalf("Execute returned unexpected: %q", out)
	}
}
