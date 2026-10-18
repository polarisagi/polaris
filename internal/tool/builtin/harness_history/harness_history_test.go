package harness_history_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/tool/builtin/harness_history"
)

type fakeOverlayManager struct {
	history []protocol.HarnessVersion
	err     error
}

func (f *fakeOverlayManager) Dir() string                       { return "/tmp" }
func (f *fakeOverlayManager) Init(ctx context.Context) error    { return nil }
func (f *fakeOverlayManager) ValidatePath(relPath string) error { return nil }
func (f *fakeOverlayManager) WriteAndCommit(ctx context.Context, files map[string][]byte, message string, author string) (string, error) {
	return "abc1234", nil
}
func (f *fakeOverlayManager) History(ctx context.Context, limit int) ([]protocol.HarnessVersion, error) {
	if f.err != nil {
		return nil, f.err
	}
	if len(f.history) > limit {
		return f.history[:limit], nil
	}
	return f.history, nil
}
func (f *fakeOverlayManager) Rollback(ctx context.Context, version string) error   { return nil }
func (f *fakeOverlayManager) GetPromptAddenda(ctx context.Context) (string, error) { return "", nil }
func (f *fakeOverlayManager) GetToolNotes(ctx context.Context) (map[string]string, error) {
	return nil, nil
}
func (f *fakeOverlayManager) GetToolNote(ctx context.Context, toolName string) (string, error) {
	return "", nil
}

func TestHarnessHistory(t *testing.T) {
	fake := &fakeOverlayManager{
		history: []protocol.HarnessVersion{
			{
				Hash:      "commit1",
				Author:    "Polaris",
				Timestamp: time.Now(),
				Message:   "First improve",
			},
		},
	}

	fn := harness_history.MakeHarnessHistoryFn(fake)
	out, err := fn(context.Background(), []byte(`{"limit": 10}`))
	if err != nil {
		t.Fatalf("MakeHarnessHistoryFn failed: %v", err)
	}

	var res struct {
		Versions []protocol.HarnessVersion `json:"versions"`
		Count    int                       `json:"count"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if res.Count != 1 || len(res.Versions) != 1 || res.Versions[0].Hash != "commit1" {
		t.Errorf("unexpected output: %+v", res)
	}
}
