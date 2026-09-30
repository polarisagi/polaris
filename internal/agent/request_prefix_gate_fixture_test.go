package agent

// ADR-0105 决策十一第二项「真实请求边界门控」的夹具：记录全部请求的 fake Provider、
// 真实 FSM + 真实 store.ImmutableCore（含契约库）+ 带 PII 的内存 MemoryFacade fake +
// 真实 PIIDetector/PIITokenVault。断言对象是**实际发往 Provider 的请求**，不是 builder 输出——
// 只测 builder 会漏掉 PromptFn 之后的一切改写（令牌化、热路径压缩、溢出恢复、PRM 候选、tools 选项）。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/polarisagi/polaris/configs"
	"github.com/polarisagi/polaris/internal/agent/fsm"
	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/memory/store"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/guard"
	"github.com/polarisagi/polaris/internal/security/taint"
	"github.com/polarisagi/polaris/internal/tool/catalog"
	"github.com/polarisagi/polaris/pkg/types"
)

// 夹具里的 PII 原文：邮箱与手机号分别出现在 L0（画像）、L1（核心记忆）、L2（历史）、L4（意图/召回画像）。
const (
	gateEmailAlice = "alice.chen@example.com"
	gatePhoneAlice = "13812345678"
	gateEmailBob   = "bob.lee@example.com"
)

var (
	gateOriginals = []string{gateEmailAlice, gatePhoneAlice, gateEmailBob}
	gatePhaseRe   = regexp.MustCompile(`(?m)^# ACTIVE PHASE: ([A-Z]+)`)
	gateTokenRe   = regexp.MustCompile(`⟦PII:[^⟧]*⟧`)
)

// gateReq 是一次实际发往 Provider 的请求。
type gateReq struct {
	turn       int
	phase      string // PERCEIVE/PLAN/REFLECT/RESPOND；无选择器为 "?"
	msgs       []types.Message
	toolsJSON  string // 无 tools 为空串
	toolNames  []string
	toolChoice string
}

// gatePhaseOf 由 L3 阶段选择器判定阶段。契约库（L0）里提到 "# ACTIVE PHASE" 的句子不在行首，
// 选择器恒在行首，故不会误判。
func gatePhaseOf(msgs []types.Message) string {
	for _, m := range msgs {
		if m.Role != "system" {
			continue
		}
		if sm := gatePhaseRe.FindStringSubmatch(m.Content); sm != nil {
			return sm[1]
		}
	}
	return "?"
}

// gateProvider 记录全部请求并按阶段出队脚本化回复。
//
// 内核请求的判据：任一消息含契约库原文（L0 的阶段契约段）。不含的是内核之外的辅助调用
// （热路径压缩的摘要、PRM 打分），不参与前缀断言，脚本键为 "aux"。
// 若某内核请求因缺陷丢了阶段选择器，它仍是内核请求、阶段记为 "?"，会被门控的层信息检查报出。
type gateProvider struct {
	mu           sync.Mutex
	turn         int
	script       map[string][]scriptedReply
	reqs         []gateReq
	aux          int
	overflowOnce map[string]bool // 阶段 → 该阶段首次请求返回 ErrContextOverflow
}

func gateIsKernel(msgs []types.Message) bool {
	section := configs.PhaseContractsSection()
	for _, m := range msgs {
		if strings.Contains(m.Content, section) {
			return true
		}
	}
	return false
}

func (p *gateProvider) next(msgs []types.Message, opts []types.InferOption) (scriptedReply, error) {
	if !gateIsKernel(msgs) {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.aux++
		if q := p.script["aux"]; len(q) > 0 {
			return q[0], nil
		}
		return scriptedReply{content: "UNSCRIPTED_aux"}, nil
	}
	var o types.InferOptions
	for _, f := range opts {
		f(&o)
	}
	phase := gatePhaseOf(msgs)
	req := gateReq{turn: 0, phase: phase, msgs: append([]types.Message(nil), msgs...), toolChoice: o.ToolChoice}
	if len(o.Tools) > 0 {
		b, _ := json.Marshal(o.Tools)
		req.toolsJSON = string(b)
		for _, ts := range o.Tools {
			req.toolNames = append(req.toolNames, ts.Name)
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	req.turn = p.turn
	p.reqs = append(p.reqs, req)
	if p.overflowOnce[phase] {
		delete(p.overflowOnce, phase)
		return scriptedReply{}, protocol.ErrContextOverflow
	}
	q := p.script[strings.ToLower(phase)]
	if len(q) == 0 {
		return scriptedReply{content: "UNSCRIPTED_" + phase}, nil
	}
	r := q[0]
	if len(q) > 1 {
		p.script[strings.ToLower(phase)] = q[1:]
	}
	return r, nil
}

func (p *gateProvider) Infer(_ context.Context, msgs []types.Message, opts ...types.InferOption) (*types.ProviderResponse, error) {
	r, err := p.next(msgs, opts)
	if err != nil {
		return nil, err
	}
	return &types.ProviderResponse{Content: r.content, ToolCalls: r.toolCalls}, nil
}

func (p *gateProvider) StreamInfer(_ context.Context, msgs []types.Message, opts ...types.InferOption) (<-chan types.StreamEvent, error) {
	r, err := p.next(msgs, opts)
	if err != nil {
		return nil, err
	}
	ch := make(chan types.StreamEvent, len(r.content)+len(r.toolCalls)+2)
	ch <- types.StreamEvent{Type: types.StreamThinking, Content: "思考中"}
	for _, chunk := range strings.SplitAfter(r.content, "。") {
		if chunk != "" {
			ch <- types.StreamEvent{Type: types.StreamTextDelta, Content: chunk}
		}
	}
	for _, tc := range r.toolCalls {
		payload, _ := json.Marshal(map[string]any{"id": tc.ID, "name": tc.Name, "input": json.RawMessage(tc.Input)})
		ch <- types.StreamEvent{Type: types.StreamToolCall, Content: string(payload)}
	}
	close(ch)
	return ch, nil
}

func (p *gateProvider) Capabilities() types.ProviderCapabilities { return types.ProviderCapabilities{} }
func (p *gateProvider) Tokenizer() protocol.TokenizerAdapter     { return nil }
func (p *gateProvider) ModelID() string                          { return "gate" }

func (p *gateProvider) snapshot() []gateReq {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]gateReq(nil), p.reqs...)
}

// perturbFn 在请求到达记录器之前改写它——模拟"PromptFn 之后某个步骤破坏了前缀"的缺陷。
// n 是该阶段第几次请求（从 0 起）。仅用于负向验证。
type perturbFn func(phase string, n int, msgs []types.Message, opts []types.InferOption) ([]types.Message, []types.InferOption)

type perturbProvider struct {
	protocol.Provider
	fn     perturbFn
	mu     sync.Mutex
	counts map[string]int
}

func (p *perturbProvider) apply(msgs []types.Message, opts []types.InferOption) ([]types.Message, []types.InferOption) {
	phase := gatePhaseOf(msgs)
	p.mu.Lock()
	n := p.counts[phase]
	p.counts[phase] = n + 1
	p.mu.Unlock()
	return p.fn(phase, n, append([]types.Message(nil), msgs...), opts)
}

func (p *perturbProvider) Infer(ctx context.Context, msgs []types.Message, opts ...types.InferOption) (*types.ProviderResponse, error) {
	m, o := p.apply(msgs, opts)
	// custom-nolint:bare-infer 测试替身：只转发，不是生产调用点
	return p.Provider.Infer(ctx, m, o...)
}

func (p *perturbProvider) StreamInfer(ctx context.Context, msgs []types.Message, opts ...types.InferOption) (<-chan types.StreamEvent, error) {
	m, o := p.apply(msgs, opts)
	// custom-nolint:bare-infer 测试替身：只转发，不是生产调用点
	return p.Provider.StreamInfer(ctx, m, o...)
}

func randHex() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// gateMemory 在集成用 mock 之上提供：真实 ImmutableCore、含 PII 的核心记忆与用户画像、可召回的情景/反思。
type gateMemory struct {
	*mockMemoryForIntegration
	core   protocol.ImmutableCore
	blocks []types.CoreMemoryBlock
}

func (m *gateMemory) ImmutableCore() protocol.ImmutableCore { return m.core }
func (m *gateMemory) ListCoreMemory(context.Context, string, string) ([]types.CoreMemoryBlock, error) {
	return m.blocks, nil
}

func (m *gateMemory) GetUserProfile(context.Context, string) (*types.UserProfile, error) {
	return &types.UserProfile{StableFacts: map[string]any{"contact": gateEmailAlice, "role": "DBA"}}, nil
}

// EpisodicProjectOf 把 FTS 命中的 gate_ev1 登记为默认项目的情景事件（召回按项目隔离反查归属）。
func (m *gateMemory) EpisodicProjectOf(_ context.Context, id string) (string, bool) {
	return types.DefaultProjectID, id == "gate_ev1"
}

// ListEpisodicEvents 只按 IDs 回取正文（ADR-0105 决策十 WP11：FTS 选 ID → 取正文）；
// 不带 IDs 的旧式整句子串查询不再有情景返回。
func (m *gateMemory) ListEpisodicEvents(_ context.Context, q types.EpisodicQuery) ([]types.ScoredEvent, error) {
	want := false
	for _, id := range q.IDs {
		want = want || id == "gate_ev1"
	}
	if !want {
		return nil, nil
	}
	return []types.ScoredEvent{{Score: 1, Event: &types.Event{
		ID:        "gate_ev1",
		Type:      "task_done",
		Payload:   []byte(`{"summary":"RECALL_EPISODIC_MARKER 上次迁移用了 goose"}`),
		CreatedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
	}}}, nil
}

func (m *gateMemory) ListReflections(context.Context, types.ReflectionQuery) ([]types.ReflectionEntry, error) {
	return []types.ReflectionEntry{{Strategy: "先备份", Decision: "RECALL_REFLECTION_MARKER 迁移前必须备份", CreatedAt: time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC)}}, nil
}

type gateCognitive struct{}

func (gateCognitive) FTSSearch(context.Context, string, int) ([]fsm.CogResult, error) {
	return []fsm.CogResult{{DocID: "sement_1", Snippet: "RECALL_SEMANTIC_MARKER 数据库迁移需要维护窗口", Score: 2}}, nil
}

// FTSEpisodic 情景召回经 BM25 选 ID；共享索引里混有语义实体文档，应被情景来源丢弃。
func (gateCognitive) FTSEpisodic(context.Context, string, int) ([]fsm.CogResult, error) {
	return []fsm.CogResult{{DocID: "gate_ev1", Score: 7}, {DocID: "sement_1", Score: 6}}, nil
}

type gateKnowledge struct{}

func (gateKnowledge) SearchRAG(context.Context, string, int) ([]fsm.KnowledgeResult, error) {
	return []fsm.KnowledgeResult{{Content: "RECALL_RAG_MARKER 迁移手册第三章", Source: "kb", Score: 0.9}}, nil
}

type gateEmbedder struct{}

func (gateEmbedder) Embed(context.Context, string) []float32 { return nil }

var gateRecallMarkers = []string{"RECALL_EPISODIC_MARKER", "RECALL_REFLECTION_MARKER", "RECALL_SEMANTIC_MARKER", "RECALL_RAG_MARKER"}

// gateEnv 是一个会话的全部共享状态：同一 session ID、同一 PII vault、同一 ImmutableCore——
// 生产里每回合一个新 Agent 实例，但这三者跨回合共享。
type gateEnv struct {
	t        *testing.T
	session  string
	vault    *guard.PIITokenVault
	detector *guard.PIIDetector
	mem      *gateMemory
	cat      *catalog.CompositeCatalog
	prov     *gateProvider
	wrap     func(protocol.Provider) protocol.Provider
	prm      *DefaultPRM
	cwm      *ContextWindowManager
}

func newGateEnv(t *testing.T) *gateEnv {
	t.Helper()
	core := store.NewImmutableCore()
	core.SoulMDContent = "You are the gate-test persona."
	core.ModelGuidance = "Prefer tool calls over guessing."
	core.PlatformHint = "Platform: test harness."
	core.BuiltinTools = "read_file, sys_probe"
	core.UserProfile = "## User Profile\nAlice Chen, reachable at " + gateEmailAlice + " or " + gatePhoneAlice + "."
	core.UserPreferences["language"] = "zh-CN"
	core.VolatileBlock = "日期 2026-09-30"
	mc := catalog.NewMemoryCatalog()
	for _, n := range []string{"read_file", "sys_probe"} {
		mc.Register(protocol.CatalogEntry{Name: n, Description: n + " tool", Parameters: map[string]any{"type": "object"}, Source: types.ToolBuiltin, TrustTier: types.TrustSystem})
	}
	for _, n := range []string{"extra_a", "extra_b"} {
		mc.Register(protocol.CatalogEntry{Name: n, Description: n + " tool", Parameters: map[string]any{"type": "object"}, Source: types.ToolMCP, TrustTier: types.TrustCommunity})
	}
	cc := catalog.NewCompositeCatalog(mc)
	cc.LazyLoadThreshold = 1 // 4 个工具 > 1：走懒加载路径（核心 + search_tools + 激活追加）
	cc.Embedder = gateEmbedder{}
	return &gateEnv{
		t:        t,
		session:  "gate-session-" + t.Name(),
		vault:    guard.NewPIITokenVault(),
		detector: guard.NewPIIDetector(),
		cat:      cc,
		prov:     &gateProvider{},
		mem: &gateMemory{
			mockMemoryForIntegration: &mockMemoryForIntegration{
				episodic: &mockEpisodicMemForIntegration{},
				working:  &mockWorkingMemForIntegration{immutable: &mockImmutableCoreForIntegration{}},
			},
			core:   core,
			blocks: []types.CoreMemoryBlock{{BlockKey: "billing", Content: "billing contact " + gateEmailBob, TaintLevel: types.TaintNone}},
		},
	}
}

// setGateThresholds 改阈值并在测试结束还原全局配置。
func setGateThresholds(t *testing.T, mut func(*config.Thresholds)) {
	t.Helper()
	prev := config.Get()
	var c config.Config
	if prev != nil {
		c = *prev
	} else {
		c.Thresholds = config.DefaultThresholds()
	}
	mut(&c.Thresholds)
	config.Update(&c)
	t.Cleanup(func() { config.Update(prev) })
}

// runTurn 以新 Agent 实例（同 session）驱动一个完整回合，返回用户可见回复；回合出错即终止测试。
func (e *gateEnv) runTurn(turn int, history []types.Message, input string, script map[string][]scriptedReply) string {
	e.t.Helper()
	reply, err := e.tryTurn(turn, history, input, script)
	if err != nil {
		e.t.Fatalf("turn %d：%v", turn, err)
	}
	return reply
}

// tryTurn 同 runTurn，但回合出错时返回 error 而不终止测试：压力场景里缺陷本身会让回合失败，
// 此时仍要对已记录的请求跑门控，才能报出首个分歧所在的层。
func (e *gateEnv) tryTurn(turn int, history []types.Message, input string, script map[string][]scriptedReply) (string, error) {
	e.t.Helper()
	e.prov.mu.Lock()
	e.prov.turn, e.prov.script = turn, script
	e.prov.mu.Unlock()

	a := NewAgentWithDefaults(e.session)
	pinSystem2Routing(e.t)
	a.surpriseCalc = fixedSurprise(0.7)
	var p protocol.Provider = e.prov
	if e.wrap != nil {
		p = e.wrap(p)
	}
	a.InjectProvider(p)
	a.InjectPolicyGate(&allowPolicyGate{})
	a.InjectToolExecutor(&mockToolExecutor{})
	a.InjectMemory(e.mem)
	a.InjectCatalog(e.cat)
	a.InjectPIITokenizer(e.detector, e.vault)
	a.SetCognitiveSearcher(gateCognitive{})
	a.SetKnowledgeSearcher(gateKnowledge{})
	if e.prm != nil {
		a.InjectPRM(e.prm)
	}
	if e.cwm != nil {
		a.InjectContextWindowManager(e.cwm)
	}
	a.SetConversationHistory(history)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sub := a.SubscribeStream(ctx)
	done := make(chan struct{})
	go func() { _ = a.Run(ctx); close(done) }()
	a.SetTaskIntent(taint.NewTaintedString(input, taint.TaintSource{OriginTaintLevel: types.TaintHigh}, "eval"))
	if err := a.SendIntent(types.TriggerIntentReceived); err != nil {
		return "", err
	}
	var reply strings.Builder
	var firstErr string
	for {
		select {
		case ev := <-sub:
			switch {
			case ev.Type == types.AgentStreamEventToken:
				reply.WriteString(ev.Content)
			case ev.Type == types.AgentStreamEventError && firstErr == "":
				firstErr = ev.Content
			case ev.Type == types.AgentStreamEventStatus && ev.Content == "task_done":
				<-done
				if firstErr != "" {
					return reply.String(), fmt.Errorf("回合出错：%s", firstErr)
				}
				if a.sm.Current() != types.AgentStateComplete {
					return reply.String(), fmt.Errorf("未以 Complete 收尾：%v", a.sm.Current())
				}
				return reply.String(), nil
			}
		case <-ctx.Done():
			return "", fmt.Errorf("未在期限内结束")
		}
	}
}

// 脚本：需要工具的回合。replan=true 时经历 Plan→Execute→Reflect(未达成)→Plan→Execute→Reflect(达成)→Respond。
func gateToolScript(replan bool) map[string][]scriptedReply {
	call := func(id string) scriptedReply {
		return scriptedReply{toolCalls: []types.InferToolCall{{ID: id, Name: "read_file", Input: json.RawMessage(`{"path":"README.md"}`)}}}
	}
	s := map[string][]scriptedReply{
		"perceive": {{content: `{"Goal":"迁移数据库并备份","NeedsTools":true}`}},
		"plan":     {call("call_1")},
		"reflect":  {{content: `{"GoalAchieved":true,"Errors":[],"Learnings":[]}`}},
		"respond":  {{content: "迁移方案已确认。"}},
	}
	if replan {
		s["plan"] = []scriptedReply{call("call_1"), call("call_2")}
		s["reflect"] = []scriptedReply{
			{content: `{"GoalAchieved":false,"Errors":["缺少备份确认"],"Learnings":[]}`},
			{content: `{"GoalAchieved":true,"Errors":[],"Learnings":[]}`},
		}
	}
	return s
}

// 脚本：Perceive 合并直答的回合（整回合只有一次请求）。
func gateDirectScript() map[string][]scriptedReply {
	return map[string][]scriptedReply{
		"perceive": {{content: `{"Goal":"确认迁移状态","NeedsTools":false,"Reply":"迁移已经完成。"}`}},
	}
}

func gateHistory(n int) []types.Message {
	h := make([]types.Message, 0, n)
	for i := 0; i < n; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		content := "历史消息 " + string(rune('A'+i%26)) + "：关于数据库迁移的讨论"
		if i == 0 {
			content = "我的邮箱是 " + gateEmailAlice + "，之前聊过数据库迁移"
		}
		h = append(h, types.Message{Role: role, Content: content})
	}
	return h
}

// gateBigHistory 构造 n 条各约 size 字节的历史（ASCII 填充，字节数精确），首条含 PII。
func gateBigHistory(n, size int) []types.Message {
	h := gateHistory(n)
	for i := range h {
		h[i].Content += " " + strings.Repeat("x", size-len(h[i].Content)-1)
	}
	return h
}
