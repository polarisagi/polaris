package agent

// 前缀稳定门控（ADR-0120 决策一，inv_PrefixStable）：用生产组装路径（fsm.BuildAgenticPrompt +
// toolInferOptions + adapter 翻译）构造连续请求，逐字节断言前缀缓存不被无谓打断。
// 命中价约为未命中价 1/50，前缀一旦在第 k 字节分歧，其后全部输入按未命中计价。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/configs"
	"github.com/polarisagi/polaris/internal/agent/fsm"
	"github.com/polarisagi/polaris/internal/llm/adapter"
	"github.com/polarisagi/polaris/internal/memory/store"
	"github.com/polarisagi/polaris/internal/prompt"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/taint"
	"github.com/polarisagi/polaris/internal/tool"
	"github.com/polarisagi/polaris/internal/tool/catalog"
	"github.com/polarisagi/polaris/pkg/types"
)

// prefixMem 只实现组装路径用到的两个方法；其余方法被调用即 panic（说明组装路径越界读了记忆）。
type prefixMem struct {
	protocol.MemoryFacade
	core   *store.ImmutableCore
	blocks []types.CoreMemoryBlock
}

func (m *prefixMem) ImmutableCore() protocol.ImmutableCore { return m.core }
func (m *prefixMem) ListCoreMemory(context.Context, string, string) ([]types.CoreMemoryBlock, error) {
	return m.blocks, nil
}

type prefixHarness struct {
	t   *testing.T
	a   *Agent
	mem *prefixMem
	cc  *catalog.CompositeCatalog
	reg *tool.InMemoryToolRegistry
}

func newPrefixCore(t *testing.T, reg *tool.InMemoryToolRegistry, cc *catalog.CompositeCatalog) *store.ImmutableCore {
	t.Helper()
	pm := prompt.NewManager(t.TempDir(), configs.FS)
	ic := store.NewImmutableCore()
	ic.SoulMDContent = pm.DefaultIdentity()
	ic.SystemPromptTemplate = pm.ReadPromptDefault("system_prompt.md")
	ic.ModelID = "deepseek-flash"
	ic.ModelGuidance = pm.ModelSpecificGuidance(ic.ModelID)
	ic.OperationalDirectives = budgetOperationalDirectives(pm)
	ic.PlatformHint = pm.PlatformHintFor("webui")
	ic.BuiltinTools = budgetBuiltinToolsHint(context.Background(), reg, cc)
	ic.VolatileBlock = "当前日期：2026-10-21"
	return ic
}

func newPrefixHarness(t *testing.T, sessionID string, shared *prefixHarness) *prefixHarness {
	t.Helper()
	h := &prefixHarness{t: t}
	if shared != nil {
		h.reg, h.cc, h.mem = shared.reg, shared.cc, shared.mem
	} else {
		h.reg = budgetRegistry(t)
		h.cc = budgetCatalog(t, h.reg)
		h.mem = &prefixMem{
			core:   newPrefixCore(t, h.reg, h.cc),
			blocks: []types.CoreMemoryBlock{{BlockKey: "user", Content: "用户偏好中文回答"}},
		}
	}
	h.a = NewAgentWithDefaults(sessionID)
	h.a.InjectCatalog(h.cc)
	h.a.sCtx.SysEnvSnapshot = "### System Environment\n- **OS**: test\n"
	return h
}

// startTurn 模拟网关一次回合入口：注入历史与意图、清空回合消息，并按生产路径写入回合首条 user 消息。
func (h *prefixHarness) startTurn(intent string, history []types.Message) {
	h.a.SetConversationHistory(history)
	h.a.sCtx.Mu.Lock()
	h.a.sCtx.RawIntentTS = taint.NewTaintedString(intent, taint.TaintSource{Module: "user", OriginTaintLevel: types.TaintHigh}, "test")
	h.a.sCtx.LoopMessages = nil
	h.a.sCtx.LoopRound = 1
	content := h.a.turnOpeningContentLocked(h.mem.core.VolatileContent(), "")
	h.a.sCtx.Mu.Unlock()
	h.a.appendLoopMessages(types.Message{Role: "user", Content: content, Round: 1})
}

// toolRound 模拟一轮工具调用：assistant tool_use + tool_result，并推进轮次。
func (h *prefixHarness) toolRound(id, output string) {
	h.a.sCtx.Mu.Lock()
	round := h.a.sCtx.LoopRound
	h.a.sCtx.Mu.Unlock()
	h.a.appendLoopMessages(assistantToolUse(round, id), makeToolResultMessage(id, output))
	h.a.sCtx.Mu.Lock()
	h.a.sCtx.LoopRound = round + 1
	h.a.sCtx.Mu.Unlock()
}

func (h *prefixHarness) request(usage string) *adapter.OpenAIRequest {
	h.t.Helper()
	ctx := context.Background()
	h.a.sCtx.Mu.RLock()
	round := append([]types.Message(nil), h.a.sCtx.LoopMessages...)
	h.a.sCtx.Mu.RUnlock()
	msgs, err := fsm.BuildAgenticPrompt(ctx, h.mem, h.a.sCtx, round, usage)
	if err != nil {
		h.t.Fatalf("BuildAgenticPrompt: %v", err)
	}
	return adapter.InspectChatRequest(msgs, h.a.toolInferOptions(ctx, types.AgentStateLoop, false)...)
}

func reqParts(r *adapter.OpenAIRequest) (tools string, msgs []string) {
	tb, _ := json.Marshal(r.Tools)
	for _, m := range r.Messages {
		b, _ := json.Marshal(m)
		msgs = append(msgs, string(b))
	}
	return string(tb), msgs
}

// sharedMessages 返回两个请求逐条相同的前导消息数；tools 不同直接返回 0（tools 位于消息之前）。
func sharedMessages(a, b *adapter.OpenAIRequest) int {
	ta, ma := reqParts(a)
	tb, mb := reqParts(b)
	if ta != tb {
		return 0
	}
	n := 0
	for n < len(ma) && n < len(mb) && ma[n] == mb[n] {
		n++
	}
	return n
}

// isCallTail 判断消息是否为调用级易变尾巴（只允许 [usage]/告警这一条落在前缀之外）。
func isCallTail(m adapter.OpenAIMessage) bool {
	s, _ := m.Content.(string)
	return m.Role == "user" && strings.HasPrefix(s, "[usage]")
}

// checkInTurn 决策一第 1 条：后一次调用以前一次调用（去掉调用级尾巴）为前缀。
func checkInTurn(prev, next *adapter.OpenAIRequest) error {
	want := len(prev.Messages)
	if want > 0 && isCallTail(prev.Messages[want-1]) {
		want--
	}
	if got := sharedMessages(prev, next); got < want {
		return fmt.Errorf("回合内前缀在第 %d 条消息分歧（需要至少 %d 条一致）：\nprev: %.300s\nnext: %.300s", got, want, msgAt(prev, got), msgAt(next, got))
	}
	return nil
}

// checkCrossTurn 决策一第 2 条：新回合首调与上回合末调共享"稳定层 + 会话层 + 上回合历史层"。
func checkCrossTurn(prevLast *adapter.OpenAIRequest, prevRoundMsgs int, next *adapter.OpenAIRequest) error {
	want := len(prevLast.Messages) - prevRoundMsgs
	if want > 0 && isCallTail(prevLast.Messages[len(prevLast.Messages)-1]) {
		want--
	}
	if got := sharedMessages(prevLast, next); got < want {
		return fmt.Errorf("跨回合前缀在第 %d 条消息分歧（需要至少 %d 条一致）：\nprev: %.300s\nnext: %.300s", got, want, msgAt(prevLast, got), msgAt(next, got))
	}
	return nil
}

func msgAt(r *adapter.OpenAIRequest, i int) string {
	if i < 0 || i >= len(r.Messages) {
		return "<none>"
	}
	b, _ := json.Marshal(r.Messages[i])
	return string(b)
}

func roundMsgCount(a *Agent) int {
	a.sCtx.Mu.RLock()
	defer a.sCtx.Mu.RUnlock()
	return len(a.sCtx.LoopMessages)
}

// 场景 A：同一回合连续三次调用。
func Test_inv_PrefixStable_InTurn(t *testing.T) {
	h := newPrefixHarness(t, "s-a", nil)
	h.startTurn("帮我看一下 main.go", nil)
	r1 := h.request("")
	h.toolRound("c1", "package main")
	r2 := h.request("[usage] tokens=100 cost≈$0.0001 wall=1.0s")
	h.toolRound("c2", "func main() {}")
	r3 := h.request("[usage] tokens=200 cost≈$0.0002 wall=2.0s")
	if err := checkInTurn(r1, r2); err != nil {
		t.Fatal(err)
	}
	if err := checkInTurn(r2, r3); err != nil {
		t.Fatal(err)
	}
}

// 场景 B：跨回合；回合 1 中途编辑核心记忆，回合间更新用户画像。
func Test_inv_PrefixStable_CrossTurn(t *testing.T) {
	h := newPrefixHarness(t, "s-b", nil)
	h.startTurn("第一个问题", nil)
	_ = h.request("")
	h.toolRound("c1", "result one")
	// 模拟 core_memory_edit：库里的块已变，但当前会话不应因此改写已发出的前缀。
	h.mem.blocks = []types.CoreMemoryBlock{{BlockKey: "user", Content: "用户偏好中文回答；喜欢表格"}}
	last := h.request("[usage] tokens=100 cost≈$0.0001 wall=1.0s")
	rounds := roundMsgCount(h.a)

	// 生产中每回合新建 Agent，并经 Pool 继承上一回合的会话层快照。
	prev := h.a
	h.a = NewAgentWithDefaults("s-b")
	h.a.InjectCatalog(h.cc)
	h.a.sCtx.SysEnvSnapshot = prev.sCtx.SysEnvSnapshot
	h.a.inheritSessionLayer(prev)
	h.startTurn("第二个问题", []types.Message{{Role: "user", Content: "第一个问题"}, {Role: "assistant", Content: "第一个回答"}})
	next := h.request("")
	if err := checkCrossTurn(last, rounds, next); err != nil {
		t.Fatal(err)
	}
}

// 场景 C：回合内经真实 tool_search 激活延迟工具后，tools 数组不变。
func Test_inv_PrefixStable_ToolActivation(t *testing.T) {
	h := newPrefixHarness(t, "s-c", nil)
	h.startTurn("看看 git 改动", nil)
	r1 := h.request("")
	search := tool.MakeToolSearchFn(h.cc, nil)
	ctx := context.WithValue(context.Background(), protocol.CtxTaskIDKey{}, "s-c")
	if _, err := search(ctx, []byte(`{"query":"git_diff"}`)); err != nil {
		t.Fatalf("tool_search: %v", err)
	}
	h.toolRound("c1", "git_diff: ...")
	r2 := h.request("[usage] tokens=100 cost≈$0.0001 wall=1.0s")
	if err := checkInTurn(r1, r2); err != nil {
		t.Fatal(err)
	}
}

// 场景 D：同配置的两个新会话，稳定层（tools + 首条系统消息）逐字节一致。
func Test_inv_PrefixStable_CrossSession(t *testing.T) {
	h1 := newPrefixHarness(t, "s-d1", nil)
	h2 := newPrefixHarness(t, "s-d2", h1)
	h1.startTurn("你好", nil)
	h2.startTurn("另一个会话的问题", nil)
	r1, r2 := h1.request(""), h2.request("")
	if got := sharedMessages(r1, r2); got < 1 {
		t.Fatalf("跨会话稳定层不一致：\n%.300s\n%.300s", msgAt(r1, 0), msgAt(r2, 0))
	}
}

// 场景 E：上下文远低于遮蔽阈值时，第 7 轮以后历史工具结果不被改写。
func Test_inv_PrefixStable_NoMaskBelowTrigger(t *testing.T) {
	h := newPrefixHarness(t, "s-e", nil)
	h.startTurn("连续执行很多小命令", nil)
	prev := h.request("")
	for i := 1; i <= 12; i++ {
		h.toolRound(fmt.Sprintf("c%d", i), fmt.Sprintf("ok %d", i))
		h.a.applyLoopObservationMasking(context.Background())
		next := h.request(fmt.Sprintf("[usage] tokens=%d cost≈$0.0001 wall=1.0s", i*100))
		if err := checkInTurn(prev, next); err != nil {
			t.Fatalf("第 %d 轮：%v", i, err)
		}
		prev = next
	}
}

// 负向用例：判据必须能识别破坏，否则门控空转（CLAUDE.md：未经负向验证的规则不算 landed）。
func Test_inv_PrefixStable_DetectorsCatchBreakage(t *testing.T) {
	h := newPrefixHarness(t, "s-neg", nil)
	h.startTurn("问题", nil)
	r1 := h.request("")
	h.toolRound("c1", "out")
	r2 := h.request("[usage] tokens=1 cost≈$0.0001 wall=1.0s")

	broken := *r2
	broken.Messages = append([]adapter.OpenAIMessage(nil), r2.Messages...)
	broken.Messages[0].Content = fmt.Sprint(broken.Messages[0].Content) + "\n时间戳 12:00:01"
	if checkInTurn(r1, &broken) == nil {
		t.Fatal("稳定层被注入时间戳后 checkInTurn 仍判通过")
	}

	brokenTools := *r2
	brokenTools.Tools = append([]adapter.OpenAITool(nil), r2.Tools...)
	brokenTools.Tools = append(brokenTools.Tools, r2.Tools[0])
	if checkInTurn(r1, &brokenTools) == nil {
		t.Fatal("tools 数组变化后 checkInTurn 仍判通过")
	}
	if checkCrossTurn(r1, 1, &broken) == nil {
		t.Fatal("稳定层被注入时间戳后 checkCrossTurn 仍判通过")
	}
	if sharedMessages(r1, &brokenTools) != 0 {
		t.Fatal("tools 不同时 sharedMessages 应为 0")
	}
}
