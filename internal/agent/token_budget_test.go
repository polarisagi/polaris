package agent

// 首轮输入 token 预算门控（ADR-0119）。
//
// 用户最在意成本：一个"你好"式的简单问题，首轮输入曾高达 ~13.9k token，其中约 75% 是 56 个工具的
// 完整定义。本测试用**生产的组装函数**拼出首轮请求的两大块并估算 token，再对合计设上限，防止回潮：
//   - tools 数组：真实注册表（RegisterBuiltinTools + 记忆/技能/委派/扩展工具）→ 与 boot_tools.go 同款
//     MemoryCatalog+CompositeCatalog（本机无 Embedder）→ Agent.toolInferOptions(Loop)。
//   - 系统提示：与 chat/system_prompt.go 同款字段填充 → ImmutableCore.PrependToMessages，加 kernel/loop.md。
//
// 估算口径与 REPORT-usage-steps-leftovers.md C5 一致（ASCII 按 3.8 字符/token，其余按 1.1 字符/token），
// 当时用此口径得到的合计与用户库实测 13862 token 吻合到 1% 以内。

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/configs"
	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/extension/native"
	"github.com/polarisagi/polaris/internal/memory/store"
	"github.com/polarisagi/polaris/internal/prompt"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/security/policy"
	"github.com/polarisagi/polaris/internal/tool"
	"github.com/polarisagi/polaris/internal/tool/builtin"
	"github.com/polarisagi/polaris/internal/tool/builtin/web_fetch"
	"github.com/polarisagi/polaris/internal/tool/catalog"
	"github.com/polarisagi/polaris/pkg/types"
)

// budgetEstTokens 估算口径同 C5：ASCII 字符 /3.8，非 ASCII（主要是汉字）/1.1。
func budgetEstTokens(s string) float64 {
	ascii, other := 0, 0
	for _, r := range s {
		if r < 128 {
			ascii++
		} else {
			other++
		}
	}
	return float64(ascii)/3.8 + float64(other)/1.1
}

type budgetGate struct{}

func (budgetGate) IsAuthorized(context.Context, string, string, string, map[string]any) (bool, error) {
	return true, nil
}

func (budgetGate) Review(context.Context, types.PolicyReviewRequest) (types.PolicyReviewResult, error) {
	return types.PolicyReviewResult{Allowed: true}, nil
}

type budgetDialer struct{ net.Dialer }

// budgetRegistry 复刻 boot_tools.go 的工具注册集合（本机实际注册 60 个）。
func budgetRegistry(t *testing.T) *tool.InMemoryToolRegistry {
	t.Helper()
	sbx := sandbox.NewInProcessSandbox(config.DefaultThresholds().M7Tool)
	reg := tool.NewInMemoryToolRegistry(sandbox.NewExecEnvelope(budgetGate{}, sandbox.NewSandboxRouter(sbx, nil, nil, "linux", 0), 0, "", nil), config.DefaultThresholds().M7Tool)
	var d protocol.SafeDialer = &budgetDialer{}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(builtin.RegisterBuiltinTools(sbx, reg, []string{t.TempDir()}, d, false, protocol.NetPolicyDeny, "", &config.Config{},
		nil, "", nil, nil, nil, web_fetch.Deps{}, policy.StandardPolicy(), "", nil, nil, nil, nil))
	must(builtin.RegisterMemoryTools(sbx, reg, nil, nil, nil, nil, nil))
	must(builtin.RegisterSkillTools(sbx, reg, nil, nil))
	must(builtin.RegisterDelegationTools(sbx, reg, nil, nil))
	// 这几个工具的执行依赖服务（automation/harness/扩展市场），元数据与生产一致即可：只量定义不执行。
	for _, n := range []string{"automation_list", "automation_save", "automation_run_now", "automation_delete", "harness_history", "harness_rollback", "tool_search"} {
		meta, err := tool.GetBuiltinToolMeta(n)
		must(err)
		must(reg.Register(meta))
	}
	for _, n := range []string{"search_extension", "install_extension"} {
		meta, err := native.GetExtensionToolMeta(n)
		must(err)
		must(reg.Register(meta))
	}
	return reg
}

// budgetCatalog 与 boot_tools.go 同款装配：MemoryCatalog ← 注册表，CompositeCatalog，无 Embedder，阈值取默认。
func budgetCatalog(t *testing.T, reg *tool.InMemoryToolRegistry) *catalog.CompositeCatalog {
	t.Helper()
	mc := catalog.NewMemoryCatalog()
	for _, tl := range reg.List() {
		mc.Register(protocol.CatalogEntry{Name: tl.Name, Description: tl.Description, Parameters: tl.InputSchema, Source: tl.Source, TrustTier: tl.TrustTier})
	}
	cc := catalog.NewCompositeCatalog(mc)
	cc.LazyLoadThreshold = config.DefaultThresholds().M13Interface.LazyLoadToolThreshold
	return cc
}

func toolsJSONTokens(tools []types.ToolSchema) (total float64, perTool map[string]float64) {
	perTool = map[string]float64{}
	for _, ts := range tools {
		b, _ := json.Marshal(map[string]any{"type": "function", "function": map[string]any{"name": ts.Name, "description": ts.Description, "parameters": ts.Parameters}})
		n := budgetEstTokens(string(b))
		perTool[ts.Name] = n
		total += n
	}
	return total, perTool
}

func TestFirstRoundBudget(t *testing.T) {
	reg := budgetRegistry(t)
	cc := budgetCatalog(t, reg)

	// ── tools 数组：Loop 状态首轮（简单问题，尚无激活工具）──
	a := NewAgentWithDefaults("budget-session")
	a.InjectCatalog(cc)
	var o types.InferOptions
	for _, f := range a.toolInferOptions(context.Background(), types.AgentStateLoop, false) {
		f(&o)
	}
	toolTok, per := toolsJSONTokens(o.Tools)

	// ── 系统提示：同 chat/system_prompt.go 的字段填充 ──
	pm := prompt.NewManager(t.TempDir(), configs.FS)
	ic := store.NewImmutableCore()
	ic.SoulMDContent = pm.DefaultIdentity()
	ic.SystemPromptTemplate = pm.ReadPromptDefault("system_prompt.md")
	ic.ModelID = "deepseek-flash"
	ic.ModelGuidance = pm.ModelSpecificGuidance(ic.ModelID)
	ic.OperationalDirectives = budgetOperationalDirectives(pm)
	ic.PlatformHint = pm.PlatformHintFor("webui")
	ic.BuiltinTools = budgetBuiltinToolsHint(context.Background(), reg, cc)
	ic.VolatileBlock = "当前日期：2026-10-20"
	msgs := ic.PrependToMessages([]types.Message{{Role: "user", Content: "你好，用一句话介绍一下你自己"}})
	var promptTok float64
	for _, m := range msgs {
		promptTok += budgetEstTokens(m.Content)
	}
	promptTok += budgetEstTokens(pm.ReadPrompt("kernel/loop.md", ""))

	names := make([]string, 0, len(per))
	for n := range per {
		names = append(names, n)
	}
	sort.Strings(names)
	var sb strings.Builder
	for _, n := range names {
		fmt.Fprintf(&sb, " %s=%.0f", n, per[n])
	}
	t.Logf("FIRST_ROUND_TOKENS tools=%.0f (%d 个) prompt=%.0f total=%.0f; 注册表共 %d 个工具", toolTok, len(o.Tools), promptTok, toolTok+promptTok, len(reg.List()))
	t.Logf("FIRST_ROUND_PER_TOOL%s", sb.String())
	t.Logf("FIRST_ROUND_HINT_TOKENS=%.0f", budgetEstTokens(ic.BuiltinTools))

	// 门控：简单问题首轮输入（估算口径，实测偏低约 3%）不得超过 7000 token；核心集不超过 15 个。
	const budgetCap = 7000
	if total := toolTok + promptTok; total > budgetCap {
		t.Errorf("首轮输入估算 %.0f token 超过上限 %d（tools=%.0f prompt=%.0f）", total, budgetCap, toolTok, promptTok)
	}
	if len(o.Tools) > 15+2 { // 核心集 ≤15，另含 Loop 追加的 code_act/finish
		t.Errorf("首轮 tools 数 %d 超过核心集上限", len(o.Tools))
	}
}

// budgetOperationalDirectives 与 chat/system_prompt.go 的 loadOperationalDirectives 同源（合并后的单文件）。
func budgetOperationalDirectives(pm *prompt.Manager) string {
	return pm.ReadPrompt("operational/directives.md", "")
}

// budgetBuiltinToolsHint 与 chat/system_prompt.go 对 ic.BuiltinTools 的填充同口径：懒加载走目录索引。
func budgetBuiltinToolsHint(ctx context.Context, reg *tool.InMemoryToolRegistry, cc *catalog.CompositeCatalog) string {
	if h := cc.ToolsHint(ctx); h != "" {
		return h
	}
	var names []string
	for _, tl := range reg.List() {
		names = append(names, tl.Name)
	}
	sort.Strings(names)
	return fmt.Sprintf("%d: %s", len(names), strings.Join(names, ", "))
}
