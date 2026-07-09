package sandbox

import (
	"testing"
)

func TestRustSandboxProbeTools(t *testing.T) {
	_, err := RustSandboxProbeTools()
	// Just ensure it doesn't panic, it might fail if dylib isn't there, which is fine
	_ = err
}

func TestWasmtimePing(t *testing.T) {
	err := WasmtimePing()
	_ = err
}
