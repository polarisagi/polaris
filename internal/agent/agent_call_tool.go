package agent

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/polarisagi/polaris/pkg/types"
)

// callToolName 常驻元工具名（ADR-0120 决策三）。
const callToolName = "call_tool"

// callToolFailure 解包失败的调用及其错误文本（以工具结果回给模型）。
type callToolFailure struct {
	call types.InferToolCall
	msg  string
}

// unwrapCallTools 把模型发出的 call_tool{name, arguments} 改写为内层调用（保留 call ID）。
//
// 为什么在内核解包：延迟加载工具不进 tools 数组（改 tools 数组会让其后全部历史失去前缀缓存），
// 模型经 call_tool 调用它们；解包放在 S_VALIDATE 之前，此后的 Profile 校验、四层校验链、
// Cedar/HITL 审批、Taint 与 ExecuteTool 都以内层工具名为主体，call_tool 不能成为绕过任何闸门的通道。
// 只改写执行用的调用；LoopMessages 里模型原样发出的 assistant tool_call 不变（历史与模型输出一致）。
func (a *Agent) unwrapCallTools(calls []types.InferToolCall) ([]types.InferToolCall, []callToolFailure) {
	out := make([]types.InferToolCall, 0, len(calls))
	var bad []callToolFailure
	for _, c := range calls {
		if c.Name != callToolName {
			out = append(out, c)
			continue
		}
		var args struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(c.Input, &args); err != nil || strings.TrimSpace(args.Name) == "" {
			bad = append(bad, callToolFailure{c, `call_tool needs {"name": "<tool>", "arguments": {...}}`})
			continue
		}
		name := strings.TrimSpace(args.Name)
		if name == callToolName {
			bad = append(bad, callToolFailure{c, "call_tool cannot call itself"})
			continue
		}
		if !a.toolKnown(name) {
			bad = append(bad, callToolFailure{c, unknownToolMessage(name, a.knownToolNames())})
			continue
		}
		input := []byte(args.Arguments)
		if len(input) == 0 || string(input) == "null" {
			input = []byte("{}")
		}
		out = append(out, types.InferToolCall{ID: c.ID, Name: name, Input: input})
	}
	return out, bad
}

// toolKnown 内层工具是否在本 Agent 可见目录中（含延迟加载工具）。无目录时放行，交给执行层判定。
func (a *Agent) toolKnown(name string) bool {
	cata := a.visibleCatalog()
	if cata == nil {
		return true
	}
	_, ok := cata.Lookup(name)
	return ok
}

func (a *Agent) knownToolNames() []string {
	cata := a.visibleCatalog()
	if cata == nil {
		return nil
	}
	entries := cata.List(a.ctx, types.TrustUntrusted)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name)
	}
	return names
}

// unknownToolMessage 未知工具名的错误文本，附最接近的 3 个名称，模型据此自行纠正。
func unknownToolMessage(name string, known []string) string {
	type scored struct {
		n string
		d int
	}
	cands := make([]scored, 0, len(known))
	for _, k := range known {
		cands = append(cands, scored{k, nameDistance(name, k)})
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].d != cands[j].d {
			return cands[i].d < cands[j].d
		}
		return cands[i].n < cands[j].n
	})
	var near []string
	for i := 0; i < len(cands) && i < 3; i++ {
		near = append(near, cands[i].n)
	}
	msg := fmt.Sprintf("unknown tool %q", name)
	if len(near) > 0 {
		msg += "; closest: " + strings.Join(near, ", ") + " (use tool_search to find tools)"
	}
	return msg
}

// nameDistance 编辑距离（工具名很短，O(n·m) 足够）。
func nameDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}
