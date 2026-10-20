package policy

import (
	"encoding/json"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/polarisagi/polaris/pkg/types"
)

// fileWriteTools 以 path 参数写文件的内置工具。
func fileWriteTools() map[string]bool {
	return map[string]bool{"write_file": true, "str_replace_editor": true, "multi_edit": true, "notebook_edit": true}
}

// uiInputActions computer_use/browser_use 中会改变外部状态的输入类动作。
func uiInputActions(tool string) map[string]bool {
	if tool == "computer_use" {
		return map[string]bool{"key": true, "type": true, "left_click": true, "right_click": true,
			"double_click": true, "left_click_drag": true, "middle_click": true, "triple_click": true}
	}
	return map[string]bool{"click": true, "type": true, "key": true, "fill": true, "select": true}
}

func parseArgs(args []byte) map[string]any {
	var m map[string]any
	if err := json.Unmarshal(args, &m); err != nil {
		return map[string]any{}
	}
	return m
}

func argString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func classifyOp(tool types.Tool, args []byte, ctx Context) operation {
	m := parseArgs(args)
	op := operation{tool: tool.Name, highRisk: tool.RiskLevel >= types.RiskHigh}
	switch tool.Name {
	case "bash", "run_command":
		return shellOp(op, m, ctx)
	case "computer_use", "browser_use":
		return uiOp(op, m)
	}
	cap := tool.EffectiveCapability()
	op.netCap = cap.AllowsNetwork()
	switch {
	case tool.Name == "str_replace_editor" && argString(m, "command") == "view":
		op.kind = opRead
	case fileWriteTools()[tool.Name]:
		op.kind = opFileWrite
		op.fillTargets(m, ctx)
	case cap >= types.CapPrivileged:
		op.kind = opPrivileged
	case cap.WritesNetwork():
		op.kind = opNetWrite
		op.host = hostOf(m)
	case cap == types.CapWriteLocal:
		op.kind = opOtherWrite
		op.fillTargets(m, ctx)
	default:
		op.kind = opRead
	}
	return op
}

func shellOp(op operation, m map[string]any, ctx Context) operation {
	op.kind = opShell
	op.netCap = true
	env := CmdEnv{Roots: ctx.Roots, Home: ctx.Home}
	cmd := argString(m, "command")
	if wd := argString(m, "working_dir"); wd != "" {
		w := newWalker(env)
		if w.pathState(wd, false) == pathInside {
			env.Cwd = wd
		} else {
			op.cmd = CmdVerdict{Class: CmdUnknown, Reason: "working_dir_outside_workspace"}
			return op
		}
	}
	if cmd == "" {
		op.cmd = CmdVerdict{Class: CmdUnknown, Reason: "empty_command"}
		return op
	}
	op.cmd = ClassifyCommand(cmd, env)
	return op
}

func uiOp(op operation, m map[string]any) operation {
	action := argString(m, "action")
	if !uiInputActions(op.tool)[action] {
		op.kind = opRead // 截图/移动/滚动等观察类动作
		return op
	}
	op.kind = opUI
	op.app = argString(m, "app", "application", "app_name")
	if op.app == "" {
		op.app = hostOf(m)
	}
	return op
}

// hostOf 从参数里取目标主机（小写，精确域名不含子域归并）。
func hostOf(m map[string]any) string {
	raw := argString(m, "url", "uri", "endpoint", "href", "target_url")
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// fillTargets 从参数里取写入目标路径并按可信根分流；取不到路径的写类工具保持无目标（视为不涉及路径）。
func (o *operation) fillTargets(m map[string]any, ctx Context) {
	w := newWalker(CmdEnv{Roots: ctx.Roots, Home: ctx.Home})
	for _, k := range []string{"path", "file_path", "notebook_path", "dir", "directory", "target"} {
		p := argString(m, k)
		if p == "" {
			continue
		}
		abs, ok := absPath(p, w.cwd, ctx.Home)
		if !ok {
			o.outside = append(o.outside, p) // 无法解析按根外处理（fail-closed）
			o.targets = append(o.targets, p)
			continue
		}
		real := resolveReal(abs)
		o.targets = append(o.targets, real)
		if !insideRoots(real, w.roots) {
			o.outside = append(o.outside, real)
		}
	}
	if o.kind == opFileWrite && len(o.targets) == 0 {
		o.outside = append(o.outside, "") // 文件写入工具缺 path：无法确认落点，按根外处理
	}
}

// coveredBy 判定该操作是否被已记住的授权完全覆盖（复合命令/多目标必须逐项命中）。
func (o operation) coveredBy(rules []Rule, mode types.PermissionMode) (string, bool) {
	switch o.kind {
	case opShell:
		return o.shellCovered(rules, mode)
	case opFileWrite, opOtherWrite:
		if len(o.targets) > 0 {
			return o.dirsCovered(rules, mode)
		}
		if o.kind == opFileWrite {
			return "", false // 缺 path 的文件写入无法确认落点，不可被记住的授权覆盖
		}
	case opNetWrite:
		if o.host != "" {
			return firstRule(rules, RuleDomain, o.host)
		}
	case opUI:
		if o.app != "" {
			if id, ok := firstRule(rules, RuleApp, o.app); ok {
				return id, true
			}
		}
	}
	return firstRule(rules, RuleTool, o.tool)
}

func firstRule(rules []Rule, kind RuleKind, pattern string) (string, bool) {
	for _, r := range rules {
		if r.Kind == kind && r.Pattern == pattern {
			return r.ID, true
		}
	}
	return "", false
}

// needSegments 需要规则覆盖的命令段：自动档只有非安全段，询问档全部段。
func (o operation) needSegments(mode types.PermissionMode) []CmdSegment {
	var out []CmdSegment
	for _, s := range o.cmd.Segments {
		if mode == types.ModeDefault || s.Class != CmdSafe {
			out = append(out, s)
		}
	}
	return out
}

func (o operation) maxSegClass() CmdClass {
	var m CmdClass
	for _, s := range o.cmd.Segments {
		if s.Class > m {
			m = s.Class
		}
	}
	return m
}

func (o operation) shellCovered(rules []Rule, mode types.PermissionMode) (string, bool) {
	// 非段来源的严重性（重定向写根外、无法解析等）不是任何前缀规则能覆盖的
	if o.cmd.Class > o.maxSegClass() || len(o.cmd.Segments) == 0 {
		return "", false
	}
	need := o.needSegments(mode)
	if len(need) == 0 {
		return "", false
	}
	var first string
	for _, seg := range need {
		id, ok := segmentRule(rules, seg)
		if !ok {
			return "", false
		}
		if first == "" {
			first = id
		}
	}
	return first, true
}

func segmentRule(rules []Rule, seg CmdSegment) (string, bool) {
	for _, r := range rules {
		if r.Kind == RuleCmd && PrefixCovers(splitPattern(r.Pattern), seg.Words) {
			return r.ID, true
		}
	}
	return "", false
}

// dirsCovered 需要覆盖的目标：自动档只有根外目标，询问档全部目标；按所在目录精确匹配 dir 规则。
func (o operation) dirsCovered(rules []Rule, mode types.PermissionMode) (string, bool) {
	need := o.outside
	if mode == types.ModeDefault {
		need = o.targets
	}
	if len(need) == 0 {
		return "", false
	}
	var first string
	for _, t := range need {
		if t == "" {
			return "", false
		}
		id, ok := firstRule(rules, RuleDir, filepath.Dir(t))
		if !ok {
			return "", false
		}
		if first == "" {
			first = id
		}
	}
	return first, true
}
