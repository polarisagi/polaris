package sandbox

import (
	"context"
	"testing"
	"github.com/polarisagi/polaris/pkg/apperr"
)

func TestWasmtimeExecute_ErrorMapping(t *testing.T) {
	if err := WasmtimeInit(); err != nil {
		t.Fatalf("WasmtimeInit failed: %v", err)
	}

	// 1. Invalid wasm binary -> WASMTIME_ERR_COMPILE (-2) -> CodeInvalidInput
	_, err := WasmtimeExecute(context.Background(), []byte("invalid wasm binary"), "{}", "", 256, false, 1000, 0, 5000)
	if err == nil {
		t.Fatal("Expected error, got nil")
	}
	if apperr.CodeOf(err) != apperr.CodeInvalidInput {
		t.Errorf("Expected CodeInvalidInput for compile error, got %v: %v", apperr.CodeOf(err), err)
	}

	// 2. FFI bridging error (e.g. invalid UTF-8 in input json) -> WASMTIME_ERR_UTF8 (-4) -> CodeInternal
	invalidUTF8 := "\xff\xfe\xfd"
	_, err2 := WasmtimeExecute(context.Background(), []byte("\x00asm\x01\x00\x00\x00"), invalidUTF8, "", 256, false, 1000, 0, 5000)
	if err2 == nil {
		t.Fatal("Expected error, got nil")
	}
	if apperr.CodeOf(err2) != apperr.CodeInvalidInput && apperr.CodeOf(err2) != apperr.CodeInternal {
		t.Errorf("Unexpected error code: %v", err2)
	}
}
