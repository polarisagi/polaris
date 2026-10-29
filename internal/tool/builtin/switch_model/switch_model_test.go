package switch_model

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
)

type mockSwitcher struct {
	current    string
	userLocked string
	switched   string
	thinking   string
}

func (m *mockSwitcher) GetModelStatus() (string, string) {
	return m.current, m.userLocked
}

func (m *mockSwitcher) SwitchModel(ref, pool, thinking string) (string, error) {
	if m.userLocked != "" {
		return "", errors.New("user locked")
	}
	target := ref
	if target == "" {
		target = pool
	}
	m.switched = target
	m.thinking = thinking
	return "ok: " + target, nil
}

func TestSwitchModel_Success(t *testing.T) {
	fn := MakeSwitchModelFn()
	switcher := &mockSwitcher{current: "default"}
	ctx := context.WithValue(context.Background(), protocol.CtxModelSwitcherKey{}, switcher)

	payload := []byte(`{"ref":"openai/gpt-4o","thinking":"high"}`)
	outBytes, err := fn(ctx, payload)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var res map[string]any
	if err := json.Unmarshal(outBytes, &res); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if res["status"] != "success" {
		t.Fatalf("expected status=success, got %v", res["status"])
	}
	if switcher.switched != "openai/gpt-4o" || switcher.thinking != "high" {
		t.Fatalf("switcher state mismatch: %+v", switcher)
	}
}

func TestSwitchModel_RejectedWhenLocked(t *testing.T) {
	fn := MakeSwitchModelFn()
	switcher := &mockSwitcher{current: "default", userLocked: "anthropic/claude-3-7-sonnet"}
	ctx := context.WithValue(context.Background(), protocol.CtxModelSwitcherKey{}, switcher)

	payload := []byte(`{"ref":"openai/gpt-4o"}`)
	outBytes, err := fn(ctx, payload)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var res map[string]any
	if err := json.Unmarshal(outBytes, &res); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if res["status"] != "rejected" {
		t.Fatalf("expected status=rejected, got %v", res["status"])
	}
	msg, _ := res["message"].(string)
	if !strings.Contains(msg, "已被用户在界面锁定") {
		t.Fatalf("expected message mentioning user locked, got %q", msg)
	}
}
