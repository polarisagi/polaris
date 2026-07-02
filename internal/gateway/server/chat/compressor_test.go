package chat

import (
	"github.com/polarisagi/polaris/internal/gateway/server/sysadmin"

	"context"
	"database/sql"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/store/repo"
	"github.com/polarisagi/polaris/pkg/types"
)

func TestCompressor_RoughTokens(t *testing.T) {
	msgs := []types.Message{
		{Content: "1234"},     // 4 chars = 1 token
		{Content: "12345678"}, // 8 chars = 2 tokens
	}
	toks := roughTokens(msgs)
	if toks != 3 {
		t.Errorf("expected 3, got %d", toks)
	}
}

func TestCompressor_NeedsCompact(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	c := NewCompressor(db, repo.NewSQLiteChatRepository(db), nil, config.CompressorConfig{
		ContextWindow:  1000,
		AutoCompactPct: 90,
	})

	if c.NeedsCompact([]types.Message{{Content: "1234"}}) {
		t.Errorf("expected false for small msg")
	}

	largeContent := strings.Repeat("a", 1000*4)
	if !c.NeedsCompact([]types.Message{{Content: largeContent}}) {
		t.Errorf("expected true for large msg")
	}
}

func TestCompressor_SplitMessages(t *testing.T) {
	msgs := []types.Message{
		{Content: strings.Repeat("a", 4000)}, // msg 0
		{Content: strings.Repeat("b", 4000)}, // msg 1
		{Content: strings.Repeat("c", 4000)}, // msg 2
	}
	// Total 12000 chars. tailTokens=2000 => 8000 chars.
	// So tail needs 8000 chars.
	// msg 2 is 4000. msg 1 + msg 2 = 8000.
	// So msg 0 goes to middle, msg 1 and msg 2 go to tail.
	middle, tail := splitMessages(msgs, 2000)
	if len(middle) != 1 || len(tail) != 2 {
		t.Errorf("expected 1 middle, 2 tail, got %d, %d", len(middle), len(tail))
	}
	if middle[0].Content[0] != 'a' {
		t.Errorf("wrong middle")
	}
}

func TestCompressor_BuildTranscript(t *testing.T) {
	msgs := []types.Message{
		{Role: "user", Content: "hello"},
	}
	ts := buildTranscript(msgs)
	if !strings.Contains(ts, "[user]: hello") {
		t.Errorf("wrong transcript: %s", ts)
	}

	large := []types.Message{
		{Role: "user", Content: strings.Repeat("a", 10000)},
	}
	ts2 := buildTranscript(large)
	if !strings.Contains(ts2, "(truncated)") {
		t.Errorf("expected truncation")
	}
}

func TestCompressor_CalcSummaryBudget(t *testing.T) {
	msgs := []types.Message{
		{Content: strings.Repeat("a", 40000)}, // 10000 tokens
	}
	// budget: 10000 * 0.20 = 2000
	budget := calcSummaryBudget(msgs)
	if budget != 2000 {
		t.Errorf("expected 2000, got %d", budget)
	}

	small := []types.Message{
		{Content: "short"},
	}
	if calcSummaryBudget(small) != minSummaryTokens {
		t.Errorf("expected %d for small", minSummaryTokens)
	}
}

func TestCompressor_Compact(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS chat_messages (session_id TEXT, role TEXT, content TEXT, file_offset INTEGER NOT NULL DEFAULT 0, file_length INTEGER NOT NULL DEFAULT 0)`)
	if err != nil {
		t.Fatal(err)
	}

	c := NewCompressor(db, repo.NewSQLiteChatRepository(db), sysadmin.NewHookRunner(""), config.CompressorConfig{
		ContextWindow:  1000,
		AutoCompactPct: 50, // threshold 500 tokens = 2000 chars
	})
	c.tailTokens = 100 // 400 chars tail

	// 3000 chars > threshold 2000 chars
	msgs := []types.Message{
		{Role: "user", Content: strings.Repeat("a", 2600)}, // goes to summary
		{Role: "user", Content: strings.Repeat("b", 400)},  // goes to tail
	}

	mp := &mockStreamProvider{
		chunks: []types.StreamEvent{
			{Type: types.StreamTextDelta, Content: "summary result"},
		},
	}

	newMsgs, res, err := c.Compact(context.Background(), "sess1", msgs, mp, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Skipped {
		t.Errorf("expected not skipped")
	}
	if len(newMsgs) != 2 {
		t.Errorf("expected 2 msgs (1 summary, 1 tail), got %d", len(newMsgs))
	}
	if !strings.Contains(newMsgs[0].Content, "summary result") {
		t.Errorf("expected summary, got %s", newMsgs[0].Content)
	}

	// Force compact
	_, _, _ = c.ForceCompact(context.Background(), "sess1", msgs, mp, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Skipped {
		t.Errorf("expected not skipped on force compact")
	}
}

type mockStreamProvider struct {
	chunks []types.StreamEvent
	err    error
}

func (m *mockStreamProvider) Infer(ctx context.Context, messages []types.Message, opts ...types.InferOption) (*types.ProviderResponse, error) {
	return &types.ProviderResponse{}, nil
}

func (m *mockStreamProvider) StreamInfer(ctx context.Context, messages []types.Message, opts ...types.InferOption) (<-chan types.StreamEvent, error) {
	if m.err != nil {
		return nil, m.err
	}
	ch := make(chan types.StreamEvent, len(m.chunks))
	for _, e := range m.chunks {
		ch <- e
	}
	close(ch)
	return ch, nil
}
func (m *mockStreamProvider) ID() string                           { return "mock" }
func (m *mockStreamProvider) Name() string                         { return "mock" }
func (m *mockStreamProvider) Close() error                         { return nil }
func (m *mockStreamProvider) ModelID() string                      { return "mock" }
func (m *mockStreamProvider) Tokenizer() protocol.TokenizerAdapter { return nil }
func (m *mockStreamProvider) Capabilities() types.ProviderCapabilities {
	return types.ProviderCapabilities{}
}
func (m *mockStreamProvider) MaxConcurrency() int             { return 1 }
func (m *mockStreamProvider) SupportsModel(model string) bool { return true }

func TestInjectTaskCanvas(t *testing.T) {
	tests := []struct {
		name    string
		mmd     string
		summary string
		want    string
	}{
		{
			name:    "empty mmd",
			mmd:     "",
			summary: "some summary",
			want:    "some summary",
		},
		{
			name:    "non-empty mmd",
			mmd:     "graph LR\n  A-->B",
			summary: "some summary",
			want:    "## Task State (node_id → read_tool_ref)\ngraph LR\n  A-->B\n## Summary\nsome summary",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := injectTaskCanvas(tt.mmd, tt.summary)
			if got != tt.want {
				t.Errorf("injectTaskCanvas() = %v, want %v", got, tt.want)
			}
		})
	}
}
