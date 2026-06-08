package native

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// ── MakeExtensionSearchFn ─────────────────────────────────────────────────────

func TestMakeExtensionSearchFn_NilBackends(t *testing.T) {
	// db=nil, client=nil → 两个后端都不可用，应报错
	fn := MakeExtensionSearchFn(nil, nil)
	input, _ := json.Marshal(map[string]string{"query": "git"})
	_, err := fn(context.Background(), input)
	if err == nil {
		t.Fatal("no backend available should return error")
	}
}

func TestMakeExtensionSearchFn_InvalidJSON(t *testing.T) {
	fn := MakeExtensionSearchFn(nil, nil)
	_, err := fn(context.Background(), []byte("not-json"))
	if err == nil {
		t.Fatal("invalid JSON input should return error")
	}
}

// ── MakeExtensionInstallFn ────────────────────────────────────────────────────

func TestMakeExtensionInstallFn_NilClient(t *testing.T) {
	fn := MakeExtensionInstallFn(nil, nil, nil, nil)
	_, err := fn(context.Background(), []byte(`{"id":"some-id"}`))
	if err == nil {
		t.Fatal("expected error with nil client")
	}
	if !strings.Contains(err.Error(), "exact package") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestMakeExtensionInstallFn_InvalidJSON(t *testing.T) {
	fn := MakeExtensionInstallFn(nil, nil, nil, nil)
	_, err := fn(context.Background(), []byte("{bad json"))
	if err == nil {
		t.Fatal("invalid JSON input should return error")
	}
}
