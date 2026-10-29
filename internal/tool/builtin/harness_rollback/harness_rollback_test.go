package harness_rollback_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/tool/builtin/harness_rollback"
	"github.com/polarisagi/polaris/pkg/apperr"
)

type fakeOverlayManager struct {
	lastRollback string
	err          error
}

func (f *fakeOverlayManager) Dir() string                       { return "/tmp" }
func (f *fakeOverlayManager) Init(ctx context.Context) error    { return nil }
func (f *fakeOverlayManager) ValidatePath(relPath string) error { return nil }
func (f *fakeOverlayManager) WriteAndCommit(ctx context.Context, files map[string][]byte, message string, author string) (string, error) {
	return "", nil
}
func (f *fakeOverlayManager) History(ctx context.Context, limit int) ([]protocol.HarnessVersion, error) {
	return nil, nil
}
func (f *fakeOverlayManager) Rollback(ctx context.Context, version string) error {
	if f.err != nil {
		return f.err
	}
	f.lastRollback = version
	return nil
}
func (f *fakeOverlayManager) GetPromptAddenda(ctx context.Context) (string, error) { return "", nil }
func (f *fakeOverlayManager) GetToolNotes(ctx context.Context) (map[string]string, error) {
	return nil, nil
}
func (f *fakeOverlayManager) GetToolNote(ctx context.Context, toolName string) (string, error) {
	return "", nil
}

type fakeAudit struct {
	action string
	target string
}

func (f *fakeAudit) Record(ctx context.Context, actor, action, target string, metadata map[string]any) error {
	f.action = action
	f.target = target
	return nil
}

func TestHarnessRollback(t *testing.T) {
	fakeMgr := &fakeOverlayManager{}
	fakeAud := &fakeAudit{}

	fn := harness_rollback.MakeHarnessRollbackFn(fakeMgr, fakeAud)

	// 1. 成功回滚
	out, err := fn(context.Background(), []byte(`{"version": "a1b2c3d"}`))
	if err != nil {
		t.Fatalf("MakeHarnessRollbackFn failed: %v", err)
	}

	var res struct {
		Success bool   `json:"success"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if !res.Success || res.Version != "a1b2c3d" {
		t.Errorf("unexpected output: %+v", res)
	}
	if fakeMgr.lastRollback != "a1b2c3d" {
		t.Errorf("expected rollback version 'a1b2c3d', got %s", fakeMgr.lastRollback)
	}
	if fakeAud.action != "harness_overlay_rollback" || fakeAud.target != "a1b2c3d" {
		t.Errorf("unexpected audit: %+v", fakeAud)
	}

	// 2. 缺少版本号
	_, err = fn(context.Background(), []byte(`{"version": ""}`))
	if err == nil || apperr.CodeOf(err) != apperr.CodeInvalidInput {
		t.Errorf("expected CodeInvalidInput for empty version, got: %v", err)
	}
}
