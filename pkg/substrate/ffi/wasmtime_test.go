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

	// minimal valid wasm module with _start export
	wasmBytes := []byte{
		0x00, 0x61, 0x73, 0x6D, 0x01, 0x00, 0x00, 0x00,
		0x01, 0x04, 0x01, 0x60, 0x00, 0x00,
		0x03, 0x02, 0x01, 0x00,
		0x07, 0x0A, 0x01, 0x06, 0x5F, 0x73, 0x74, 0x61, 0x72, 0x74, 0x00, 0x00,
		0x0A, 0x04, 0x01, 0x02, 0x00, 0x0B,
	}
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
