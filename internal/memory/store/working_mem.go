package store

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"text/template"

	"github.com/polarisagi/polaris/pkg/apperr"

	"github.com/polarisagi/polaris/internal/prompt"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// ============================================================================
// WorkingMemory (L0) — 进程内，非持久化
// ============================================================================

type WorkingMem struct {
	immutable   *ImmutableCore
	context     *ContextWindowImpl
	scratch     *ScratchPadImpl
	notes       protocol.NotesStore
	tokenBudget int          // 0 = 禁用自动换页
	episodic    *EpisodicMem // 换页目标，nil = 禁用换页
}

func NewWorkingMem() *WorkingMem {
	return &WorkingMem{
		immutable: NewImmutableCore(),
		context:   NewContextWindow(100),
		scratch:   NewScratchPad(),
		notes:     NewInMemNotesStore(),
	}
}

// NewWorkingMemWithDB 创建含 SQL 持久化 NotesStore 的 WorkingMem。
func NewWorkingMemWithDB(db protocol.SQLQuerier) *WorkingMem {
	w := NewWorkingMem()
	if db != nil {
		w.notes = NewSQLNotesStore(db)
	}
	return w
}

// NewWorkingMemWithBudget 创建带 token budget 的 WorkingMem，超出阈值自动换页到 EpisodicMem。
// budget: token 上限（Tier 0 推荐 8000，Tier 1 推荐 32000）。
// episodic: 换页目标，nil 时退化为普通 WorkingMem（仅压缩，不换页）。
func NewWorkingMemWithBudget(db protocol.SQLQuerier, episodic *EpisodicMem, budget int) *WorkingMem {
	w := NewWorkingMemWithDB(db)
	w.tokenBudget = budget
	w.episodic = episodic
	return w
}

func (w *WorkingMem) Immutable() protocol.ImmutableCore { return w.immutable }
func (w *WorkingMem) Context() protocol.ContextWindow   { return w.context }
func (w *WorkingMem) Scratch() protocol.ScratchPad      { return w.scratch }
func (w *WorkingMem) Notes() protocol.NotesStore        { return w.notes }

// AppendAndPage 追加消息，超过 tokenBudget 时自动换页到 EpisodicMem。
// agent_execute.go 的消息追加调用此方法。
func (w *WorkingMem) AppendAndPage(ctx context.Context, msg types.Message) {
	w.context.Append(msg)
	if w.tokenBudget <= 0 || w.episodic == nil {
		return
	}
	if w.context.Tokens() > w.tokenBudget {
		if err := CompactWorkingMemory(ctx, w.context, w.episodic, w.tokenBudget*3/4); err != nil {
			// 换页失败不阻断主流程，记录 warn
			slog.Warn("working memory compaction failed", "err", err)
		}
	}
}

func (ic *ImmutableCore) Load(ctx context.Context, userID, sessionID string) (types.ImmutableCoreView, error) {
	var prefs []types.UserPreference //nolint:prealloc
	for k, v := range ic.UserPreferences {
		prefs = append(prefs, types.UserPreference{
			Dimension:      k,
			PreferenceText: v,
			Confidence:     1.0,
		})
	}
	return types.ImmutableCoreView{
		SessionGoal: ic.GlobalGoal,
		UserPrefs:   prefs,
	}, nil
}

func (ic *ImmutableCore) renderSystemPrompt() string {
	// M9 / 用户自定义模板：全量委托给模板渲染，跳过三层组装
	if ic.SystemPromptTemplate != "" {
		t, err := template.New("sys").Parse(ic.SystemPromptTemplate)
		if err != nil {
			return "Error parsing system prompt: " + err.Error() + "\n"
		}
		var buf bytes.Buffer
		if err := t.Execute(&buf, ic); err != nil {
			return "Error rendering system prompt: " + err.Error() + "\n"
		}
		return buf.String()
	}

	// 三层组装：stable → model guidance → platform hint → volatile
	var parts []string

	// 1. stable — 身份（SoulMDContent 已由 server 按三层优先级填充）
	if ic.SoulMDContent != "" {
		parts = append(parts, ic.SoulMDContent)
	} else {
		// server 未注入时的最终兜底（不应触发，仅防御性保护）
		parts = append(parts, prompt.DefaultPolarisIdentityFallback)
	}

	// 2. stable — 模型专属工具调用引导
	if ic.ModelGuidance != "" {
		parts = append(parts, ic.ModelGuidance)
	}

	// 3. stable — 用户自定义追加指令（追加而非覆盖，保留产品基线行为）
	if ic.CustomInstructions != "" {
		parts = append(parts, ic.CustomInstructions)
	}

	// 4. stable — 平台感知提示
	if ic.PlatformHint != "" {
		parts = append(parts, ic.PlatformHint)
	}

	// 5. stable — 工具/扩展感知摘要（仅名称，细节由 function schema 传递）
	if ic.BuiltinTools != "" || ic.InstalledPlugins != "" {
		var toolParts []string
		if ic.BuiltinTools != "" {
			toolParts = append(toolParts, "Built-in tools: "+ic.BuiltinTools)
		}
		if ic.InstalledPlugins != "" {
			toolParts = append(toolParts, "Extensions: "+ic.InstalledPlugins)
		}
		toolHint := "You have tools callable via the function-call API.\n" + strings.Join(toolParts, "\n")
		parts = append(parts, toolHint)
	}

	// 6. volatile — 时间戳 / 会话信息（精确到天，不破坏 prefix cache）
	if ic.VolatileBlock != "" {
		parts = append(parts, ic.VolatileBlock)
	}

	return strings.Join(parts, "\n\n")
}

// maxSystemPromptBytes 系统提示词单次渲染的字节数硬性上限（≈8K tokens at 4 chars/token）。
// 超出部分截断并记录 warn，防止大量插件/工具文本撑爆 LLM context window。
// ambient skill 全文注入有独立的 maxFullTextChars 预算，两者各自独立保护。
const maxSystemPromptBytes = 32_000

func (ic *ImmutableCore) PrependToMessages(msgs []types.Message) []types.Message {
	content := ic.renderSystemPrompt()

	// 去除多余的尾部换行
	content = strings.TrimRight(content, "\n")

	// 如果全部为空，给一个默认提示词
	if content == "" {
		content = "你是 Polaris AI Agent。"
	}

	// 系统提示词硬性截断：防止大量插件/工具文本撑爆 context window（仅限 stable+volatile 层）
	if len(content) > maxSystemPromptBytes {
		originalBytes := len(content)
		truncated := content[:maxSystemPromptBytes]
		// 截到最后一个完整段落（至少保留前半部分），避免在段中截断
		if idx := strings.LastIndex(truncated, "\n\n"); idx > maxSystemPromptBytes/2 {
			truncated = truncated[:idx]
		}
		content = truncated + "\n\n[...系统提示词已截断]"
		slog.Warn("system prompt truncated",
			"original_bytes", originalBytes, "cap_bytes", maxSystemPromptBytes)
	}

	// AmbientContext 在模板渲染完成后追加，不经过 Go template 解析器。
	// 这样 skill instructions 含 {{ }} 时不会破坏模板解析（Bug-fix: template injection）。
	// AmbientContext 有独立的 maxFullTextChars(128K) 预算，不纳入上方截断逻辑。
	if ic.AmbientContext != "" {
		content += ic.AmbientContext
	}

	return append([]types.Message{{Role: "system", Content: content}}, msgs...)
}

// ContextWindowImpl 上下文窗口管理（环形缓冲区 + 不可变核心区保护）。
type ContextWindowImpl struct {
	messages  []types.Message
	capacity  int
	mu        sync.Mutex
	maxTokens int
}

func NewContextWindow(capacity int) *ContextWindowImpl {
	return &ContextWindowImpl{
		messages:  make([]types.Message, 0, capacity),
		capacity:  capacity,
		maxTokens: 128000,
	}
}

func (cw *ContextWindowImpl) Append(msg types.Message) {
	cw.mu.Lock()
	defer cw.mu.Unlock()
	cw.messages = append(cw.messages, msg)
	// 超容量时驱逐最低分的非 system 消息
	if len(cw.messages) > cw.capacity {
		cw.evictByScore()
	}
}

// Compress 将上下文压缩到 targetTokens 以内。
// 保护规则: role=="system" 消息绝对不驱逐（ImmutableCore 区）。
// 驱逐顺序: 按重要度评分升序（最低分先删）。
func (cw *ContextWindowImpl) Compress(ctx context.Context, targetTokens int) error {
	cw.mu.Lock()
	defer cw.mu.Unlock()
	cw.maxTokens = targetTokens
	for cw.tokenCount() > targetTokens {
		removed := cw.evictByScore()
		if !removed {
			break // 仅剩 system 消息，无法继续压缩
		}
	}
	return nil
}

// evictByScore 驱逐重要度最低的一条非 system 消息。
// 评分规则:
//   - system: math.MaxFloat64（不可驱逐）
//   - 最近 5 条非 system: 基础分 × 2.0
//   - role=="tool": 基础分 × 0.5（工具输出价值较低）
//   - 其余: 基础分 = 1.0
//
// 返回是否发生了驱逐。
func (cw *ContextWindowImpl) evictByScore() bool {
	n := len(cw.messages)
	lowestIdx := -1
	lowestScore := 1e18

	recentThreshold := n - 5
	for i, msg := range cw.messages {
		if msg.Role == "system" {
			continue
		}
		score := 1.0
		if msg.Role == "tool" {
			score *= 0.5
		}
		if i >= recentThreshold {
			score *= 2.0
		}
		if score < lowestScore {
			lowestScore = score
			lowestIdx = i
		}
	}
	if lowestIdx < 0 {
		return false
	}
	// 切除该消息
	cw.messages = append(cw.messages[:lowestIdx], cw.messages[lowestIdx+1:]...)
	return true
}

// tokenCount 估算当前 token 数（4 字符≈1 token）。
func (cw *ContextWindowImpl) tokenCount() int {
	total := 0
	for _, m := range cw.messages {
		total += len(m.Content)/4 + 4 // +4 for role overhead
	}
	return total
}

func (cw *ContextWindowImpl) Tokens() int {
	cw.mu.Lock()
	defer cw.mu.Unlock()
	return cw.tokenCount()
}

func (cw *ContextWindowImpl) Messages() []types.Message {
	cw.mu.Lock()
	defer cw.mu.Unlock()
	msgs := make([]types.Message, len(cw.messages))
	copy(msgs, cw.messages)
	return msgs
}

// CompactWorkingMemory 压缩 ContextWindow 至 targetTokens，
// 将被驱逐的消息导出为 EpisodicMem 事件（冷路径异步持久化）。
func CompactWorkingMemory(ctx context.Context, cw *ContextWindowImpl, em *EpisodicMem, targetTokens int) error {
	cw.mu.Lock()
	// 记录压缩前快照，找出哪些消息会被驱逐
	before := make([]types.Message, len(cw.messages))
	copy(before, cw.messages)
	cw.mu.Unlock()

	if err := cw.Compress(ctx, targetTokens); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "CompactWorkingMemory", err)
	}

	cw.mu.Lock()
	after := make(map[int]bool)
	for i := range cw.messages {
		after[i] = true
	}
	cw.mu.Unlock()

	// 将被驱逐消息导出到 EpisodicMem（尽力而为，不阻断主流程）
	afterMsgs := cw.Messages()
	afterSet := make(map[string]bool)
	for _, m := range afterMsgs {
		afterSet[m.Role+":"+m.Content] = true
	}
	for _, m := range before {
		if m.Role == "system" {
			continue
		}
		key := m.Role + ":" + m.Content
		if !afterSet[key] && em != nil {
			ev := types.Event{
				ID:      "compact_" + m.Role + "_" + string(rune(len(m.Content))),
				Type:    "working_memory_evicted",
				TaskID:  "compact",
				Payload: []byte(m.Content),
			}
			_ = em.Append(ctx, ev, types.TaintNone)
		}
	}
	return nil
}

// ScratchPadImpl 任务级临时键值存储，goroutine-safe。
type ScratchPadImpl struct {
	data sync.Map
}

func NewScratchPad() *ScratchPadImpl {
	return &ScratchPadImpl{}
}

func (sp *ScratchPadImpl) Set(key string, value any)  { sp.data.Store(key, value) }
func (sp *ScratchPadImpl) Get(key string) (any, bool) { return sp.data.Load(key) }
func (sp *ScratchPadImpl) Clear()                     { sp.data = sync.Map{} }

func (w *WorkingMem) TokenBudget() int { return w.tokenBudget }

func (w *WorkingMem) Episodic() *EpisodicMem { return w.episodic }

func (w *WorkingMem) ContextWindow() *ContextWindowImpl { return w.context }

func (w *WorkingMem) SetTokenBudget(b int)       { w.tokenBudget = b }
func (w *WorkingMem) SetEpisodic(e *EpisodicMem) { w.episodic = e }
