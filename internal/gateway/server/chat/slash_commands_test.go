package chat

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/polarisagi/polaris/pkg/types"
)

// ── parseSlashCommand ─────────────────────────────────────────────────────────

func TestParseSlashCommand_Valid(t *testing.T) {
	cases := []struct {
		input   string
		wantCmd string
		wantOK  bool
	}{
		{"/help", "/help", true},
		{" /compact  ", "/compact", true},
		{"/CONTEXT", "/context", true},     // 大小写不敏感
		{"/compact now", "/compact", true}, // 带参数
		{"hello", "", false},               // 普通消息
		{"//comment", "", false},           // URL 风格不识别
		{"/", "/", true},                   // 裸斜线（路由为未知命令）
	}
	for _, tc := range cases {
		cmd, _, ok := parseSlashCommand(tc.input)
		if ok != tc.wantOK {
			t.Errorf("parseSlashCommand(%q) ok=%v want %v", tc.input, ok, tc.wantOK)
		}
		if ok && cmd != tc.wantCmd {
			t.Errorf("parseSlashCommand(%q) cmd=%q want %q", tc.input, cmd, tc.wantCmd)
		}
	}
}

// ── SlashCommandRouter.Dispatch ───────────────────────────────────────────────

// mockFlusher 实现 http.Flusher 接口（no-op）。
type mockFlusher struct{ http.ResponseWriter }

func (m mockFlusher) Flush() {}

func newTestRouter(t *testing.T) (*SlashCommandRouter, *httptest.ResponseRecorder, mockFlusher) {
	t.Helper()
	router := &SlashCommandRouter{
		compressor: &Compressor{
			contextWindow:  defaultContextWindow,
			autoCompactPct: defaultAutoCompactPct,
			warnPct:        defaultWarnPct,
			maxThrashCount: defaultMaxThrashCount,
			tailTokens:     defaultTailTokens,
		},
		chatRepo: nil,
		writeSSE: func(w http.ResponseWriter, f http.Flusher, event string, data any) {},
	}
	rec := httptest.NewRecorder()
	flusher := mockFlusher{rec}
	return router, rec, flusher
}

func testHistory() []types.Message {
	return []types.Message{
		{Role: "system", Content: "你是 Polaris。"},
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "hi"},
	}
}

func TestDispatch_NotSlashCommand(t *testing.T) {
	router, rec, flusher := newTestRouter(t)
	history := testHistory()
	result := router.Dispatch(context.Background(), "普通消息", "s1", history, nil, rec, flusher)
	if result.Handled {
		t.Fatal("普通消息不应被 Dispatch 处理")
	}
}

func TestDispatch_Help(t *testing.T) {
	router, rec, flusher := newTestRouter(t)
	result := router.Dispatch(context.Background(), "/help", "s1", testHistory(), nil, rec, flusher)
	if !result.Handled {
		t.Fatal("/help 应被处理")
	}
	if result.Response == "" {
		t.Fatal("/help 回复不应为空")
	}
	// 检查所有注册命令都出现在回复中
	for _, cmd := range builtinSlashCommands() {
		if !contains(result.Response, cmd.Name) {
			t.Errorf("/help 回复缺少命令 %s", cmd.Name)
		}
	}
}

func TestDispatch_Context(t *testing.T) {
	router, rec, flusher := newTestRouter(t)
	history := testHistory()
	result := router.Dispatch(context.Background(), "/context", "sess-42", history, nil, rec, flusher)
	if !result.Handled {
		t.Fatal("/context 应被处理")
	}
	if !contains(result.Response, "sess-42") {
		t.Error("/context 回复应包含 session id")
	}
	if !contains(result.Response, "token") && !contains(result.Response, "Token") {
		t.Error("/context 回复应包含 token 统计信息")
	}
}

func TestDispatch_Clear(t *testing.T) {
	router, rec, flusher := newTestRouter(t)
	history := testHistory()
	result := router.Dispatch(context.Background(), "/clear", "s1", history, nil, rec, flusher)
	if !result.Handled {
		t.Fatal("/clear 应被处理")
	}
	// 仅保留 system 消息
	for _, m := range result.UpdatedHistory {
		if m.Role != "system" {
			t.Errorf("/clear 后 history 中不应有 role=%s 的消息", m.Role)
		}
	}
}

func TestDispatch_CompactNoProvider(t *testing.T) {
	router, rec, flusher := newTestRouter(t)
	result := router.Dispatch(context.Background(), "/compact", "s1", testHistory(), nil, rec, flusher)
	if !result.Handled {
		t.Fatal("/compact 应被处理（即使无 provider）")
	}
	if !contains(result.Response, "未配置") {
		t.Error("/compact 无 provider 时应提示未配置")
	}
}

func TestDispatch_UnknownCommand(t *testing.T) {
	router, rec, flusher := newTestRouter(t)
	result := router.Dispatch(context.Background(), "/unknowncmd", "s1", testHistory(), nil, rec, flusher)
	if !result.Handled {
		t.Fatal("未知斜线命令应被拦截（Handled=true）")
	}
	if !contains(result.Response, "未知命令") {
		t.Error("未知命令应提示 '未知命令'")
	}
}

// ── Compressor.Stats ──────────────────────────────────────────────────────────

func TestCompressor_Stats_Empty(t *testing.T) {
	c := &Compressor{contextWindow: 1000, autoCompactPct: defaultAutoCompactPct, warnPct: defaultWarnPct, maxThrashCount: defaultMaxThrashCount, tailTokens: defaultTailTokens}
	stats := c.Stats(nil)
	if stats.TokenCount != 0 {
		t.Errorf("空 history token=%d want 0", stats.TokenCount)
	}
	if stats.UsagePercent != 0 {
		t.Errorf("空 history usage=%.1f want 0", stats.UsagePercent)
	}
	if !stats.LastCompactAt.IsZero() {
		t.Error("从未压缩时 LastCompactAt 应为零值")
	}
}

func TestCompressor_Stats_Threshold(t *testing.T) {
	// contextWindow=100 token，autoCompactPct=100% → 阈值=100 token
	c := &Compressor{contextWindow: 100, autoCompactPct: 100.0, warnPct: defaultWarnPct, maxThrashCount: defaultMaxThrashCount, tailTokens: defaultTailTokens}
	// 200 字符 = 50 token（roughTokens: chars/4）
	msgs := []types.Message{{Role: "user", Content: repeat("x", 200)}}
	stats := c.Stats(msgs)
	if stats.TokenCount != 50 {
		t.Errorf("token=%d want 50", stats.TokenCount)
	}
	if stats.UsagePercent != 50.0 {
		t.Errorf("usage=%.1f want 50.0", stats.UsagePercent)
	}
}

// ── splitMessages ─────────────────────────────────────────────────────────────

func TestSplitMessages_TailCoversAll(t *testing.T) {
	msgs := []types.Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "hello"},
	}
	middle, tail := splitMessages(msgs, 1000)
	if len(middle) != 0 {
		t.Errorf("middle len=%d want 0", len(middle))
	}
	if len(tail) != 2 {
		t.Errorf("tail len=%d want 2", len(tail))
	}
}

func TestSplitMessages_LargeHistory(t *testing.T) {
	// 构造 10 条消息，每条 40 字符 = 10 token
	msgs := make([]types.Message, 0, 10)
	for range 10 {
		msgs = append(msgs, types.Message{Role: "user", Content: repeat("x", 40)})
	}
	// tailTokens=20 token = 5 条消息
	middle, tail := splitMessages(msgs, 20)
	if len(tail) == 0 {
		t.Fatal("tail 不应为空")
	}
	if len(middle)+len(tail) != 10 {
		t.Errorf("middle(%d)+tail(%d) != 10", len(middle), len(tail))
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 ||
		func() bool {
			for i := 0; i <= len(s)-len(substr); i++ {
				if s[i:i+len(substr)] == substr {
					return true
				}
			}
			return false
		}())
}

func repeat(s string, n int) string {
	b := make([]byte, n*len(s))
	for i := range b {
		b[i] = s[0]
	}
	return string(b)
}
