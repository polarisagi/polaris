package consolidation

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/memory/store"
	"github.com/polarisagi/polaris/internal/memory/testutil"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

type profileTrackingSemantic struct {
	protocol.SemanticMemory
	getKeys  []string
	upserted []types.UserProfile
	getErr   error
}

func (m *profileTrackingSemantic) GetUserProfile(ctx context.Context, key string) (*types.UserProfile, error) {
	m.getKeys = append(m.getKeys, key)
	if m.getErr != nil {
		return nil, m.getErr
	}
	return m.SemanticMemory.GetUserProfile(ctx, key)
}

func (m *profileTrackingSemantic) UpsertUserProfile(ctx context.Context, profile types.UserProfile) error {
	m.upserted = append(m.upserted, profile)
	return m.SemanticMemory.UpsertUserProfile(ctx, profile)
}

type profileCountingSummarizer struct {
	calls    int
	prompts  []string
	response string
	err      error
}

func (s *profileCountingSummarizer) Summarize(ctx context.Context, prompt string, maxTokens int) (string, error) {
	return s.InferRaw(ctx, prompt, maxTokens)
}

func (s *profileCountingSummarizer) InferRaw(_ context.Context, prompt string, _ int) (string, error) {
	s.calls++
	s.prompts = append(s.prompts, prompt)
	if s.err != nil {
		return "", s.err
	}
	if s.response != "" {
		return s.response, nil
	}
	return `{"stable_facts":{"source":"low-default"},"behavioral_patterns":{}}`, nil
}

func profileEvents(n int, project string, taint types.TaintLevel, payload string) []types.ScoredEvent {
	events := make([]types.ScoredEvent, 0, n)
	for i := range n {
		events = append(events, types.ScoredEvent{Event: types.Event{
			ID: fmt.Sprintf("e-%s-%d", project, i), Type: "tool_call", Payload: []byte(payload),
			ProjectID: project, TaintLevel: taint, CreatedAt: time.Now(),
		}})
	}
	return events
}

func TestSynthesizeUserProfileFiltersBeforeReadAndSynthesis(t *testing.T) {
	ctx := context.Background()
	semantic := &profileTrackingSemantic{SemanticMemory: store.NewSemanticMem(testutil.NewMockStore(), &testutil.MockIntentSubmitter{})}
	summarizer := &profileCountingSummarizer{}
	pipe := NewConsolidationPipelineFull(nil, semantic, nil, summarizer, nil, nil, nil)

	// 不足阈值的默认低污点事件，叠加足量项目/高污点/空内容事件仍不得访问画像存储或模型。
	events := profileEvents(9, types.DefaultProjectID, types.TaintLow, "eligible")
	events = append(events, profileEvents(12, "project-a", types.TaintLow, "project")...)
	events = append(events, profileEvents(12, types.DefaultProjectID, types.TaintHigh, "high")...)
	events = append(events, profileEvents(12, types.DefaultProjectID, types.TaintLow, " ")...)
	if err := pipe.synthesizeUserProfile(ctx, events); err != nil {
		t.Fatal(err)
	}
	if len(semantic.getKeys) != 0 || len(semantic.upserted) != 0 || summarizer.calls != 0 {
		t.Fatalf("filtered batch must not read/write profile or invoke summarizer: reads=%v upserts=%d calls=%d", semantic.getKeys, len(semantic.upserted), summarizer.calls)
	}
}

func TestSynthesizeUserProfileReadFailureStopsSynthesisAndWrite(t *testing.T) {
	semantic := &profileTrackingSemantic{
		SemanticMemory: store.NewSemanticMem(testutil.NewMockStore(), &testutil.MockIntentSubmitter{}),
		getErr:         fmt.Errorf("profile database unavailable"),
	}
	summarizer := &profileCountingSummarizer{}
	pipe := NewConsolidationPipelineFull(nil, semantic, nil, summarizer, nil, nil, nil)

	err := pipe.synthesizeUserProfile(context.Background(), profileEvents(10, types.DefaultProjectID, types.TaintLow, "eligible"))
	if err == nil {
		t.Fatal("profile read failure must be surfaced")
	}
	if len(semantic.getKeys) != 1 || semantic.getKeys[0] != types.GlobalUserProfileKey {
		t.Fatalf("expected one read through versioned key, got %v", semantic.getKeys)
	}
	if summarizer.calls != 0 || len(semantic.upserted) != 0 {
		t.Fatalf("read failure must stop synthesis and write: calls=%d upserts=%d", summarizer.calls, len(semantic.upserted))
	}
}

func TestSynthesizeUserProfileUsesOnlyLowTaintDefaultEventsAndVersionedKey(t *testing.T) {
	ctx := context.Background()
	backingSemantic := store.NewSemanticMem(testutil.NewMockStore(), &testutil.MockIntentSubmitter{})
	if err := backingSemantic.UpsertUserProfile(ctx, types.UserProfile{ProfileKey: "default", StableFacts: map[string]any{"legacy": true}}); err != nil {
		t.Fatal(err)
	}
	semantic := &profileTrackingSemantic{SemanticMemory: backingSemantic}
	summarizer := &profileCountingSummarizer{}
	pipe := NewConsolidationPipelineFull(nil, semantic, nil, summarizer, nil, nil, nil)
	events := profileEvents(10, "", types.TaintLow, "eligible")
	events = append(events, profileEvents(10, "project-b", types.TaintLow, "must not enter synthesis")...)
	events = append(events, profileEvents(10, types.DefaultProjectID, types.TaintHigh, "must not enter synthesis")...)

	if err := pipe.synthesizeUserProfile(ctx, events); err != nil {
		t.Fatal(err)
	}
	if len(semantic.getKeys) != 1 || semantic.getKeys[0] != types.GlobalUserProfileKey {
		t.Fatalf("profile lookup must use only versioned key: got %v", semantic.getKeys)
	}
	if len(semantic.upserted) != 1 || semantic.upserted[0].ProfileKey != types.GlobalUserProfileKey {
		t.Fatalf("profile write must use versioned key: %+v", semantic.upserted)
	}
	if summarizer.calls != 1 {
		t.Fatalf("eligible batch should invoke summarizer once, got %d", summarizer.calls)
	}
	if !strings.Contains(summarizer.prompts[0], "eligible") || strings.Contains(summarizer.prompts[0], "must not enter synthesis") {
		t.Fatalf("synthesis prompt must contain only eligible events: %q", summarizer.prompts[0])
	}
	profile, err := semantic.GetUserProfile(ctx, types.GlobalUserProfileKey)
	if err != nil {
		t.Fatal(err)
	}
	if profile.StableFacts["source"] != "low-default" {
		t.Fatalf("expected filtered low-taint event synthesis, got %+v", profile.StableFacts)
	}
	legacy, err := backingSemantic.GetUserProfile(ctx, "default")
	if err != nil || legacy.StableFacts["legacy"] != true {
		t.Fatalf("historical default profile must remain untouched, profile=%+v err=%v", legacy, err)
	}
}

func TestSynthesizeUserProfileFallbackLogsMetadataOnly(t *testing.T) {
	tests := []struct {
		name     string
		provider *profileCountingSummarizer
		wantOp   string
	}{
		{name: "provider failure", provider: &profileCountingSummarizer{err: fmt.Errorf("provider-private-cause")}, wantOp: "provider_infer"},
		{name: "invalid response", provider: &profileCountingSummarizer{response: `RESPONSE-PRIVATE-MARKER`}, wantOp: "response_decode"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var logs bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			defer slog.SetDefault(previous)

			semantic := &profileTrackingSemantic{SemanticMemory: store.NewSemanticMem(testutil.NewMockStore(), &testutil.MockIntentSubmitter{})}
			pipe := NewConsolidationPipelineFull(nil, semantic, nil, tt.provider, nil, nil, nil)
			payload := "EVENT-PRIVATE-MARKER"
			if err := pipe.synthesizeUserProfile(context.Background(), profileEvents(10, types.DefaultProjectID, types.TaintLow, payload)); err != nil {
				t.Fatal(err)
			}
			if len(semantic.upserted) != 1 || len(semantic.upserted[0].RecentActivity) == 0 {
				t.Fatalf("rule fallback should still synthesize a profile: %+v", semantic.upserted)
			}
			logText := logs.String()
			for _, want := range []string{tt.wantOp, "fallback=rule", "error_type="} {
				if !strings.Contains(logText, want) {
					t.Fatalf("missing structured fallback field %q in %q", want, logText)
				}
			}
			for _, secret := range []string{payload, "provider-private-cause", "RESPONSE-PRIVATE-MARKER"} {
				if strings.Contains(logText, secret) {
					t.Fatalf("fallback log leaked private content %q: %s", secret, logText)
				}
			}
		})
	}
}
