package policy

import (
	"path/filepath"
	"strings"

	"github.com/polarisagi/polaris/pkg/types"
)

// ask 生成"询问"判定：原因码 + 勾选"以后不再询问"时将创建的最窄规则与展示文案。
func (o operation) ask(mode types.PermissionMode) Decision {
	d := Decision{Verdict: VerdictAsk, Reason: o.askReason(mode)}
	d.Rules, d.RememberLabel = o.suggest(mode)
	return d
}

func (o operation) askReason(mode types.PermissionMode) string {
	switch o.kind {
	case opFileWrite, opOtherWrite:
		if len(o.outside) > 0 {
			return ReasonOutsideWorkspaceWrite
		}
		if o.highRisk {
			return ReasonHighRiskTool
		}
	case opShell:
		switch o.cmd.Class {
		case CmdDangerous, CmdCatastrophic:
			return ReasonDangerousCommand
		case CmdUnknown:
			return ReasonUnknownCommand
		}
	case opNetWrite:
		return ReasonNetworkWrite
	case opPrivileged:
		return ReasonPrivileged
	case opUI:
		return ReasonUIControl
	}
	return ReasonAskMode
}

// suggest 返回最窄规则及展示文案；无可记住项时返回 (nil, "")。
func (o operation) suggest(mode types.PermissionMode) ([]Rule, string) {
	switch o.kind {
	case opShell:
		return o.suggestCmd(mode)
	case opFileWrite, opOtherWrite:
		if len(o.targets) > 0 {
			return o.suggestDirs(mode)
		}
		if o.kind == opFileWrite {
			return nil, ""
		}
	case opNetWrite:
		if o.host != "" {
			return []Rule{{Kind: RuleDomain, Pattern: o.host}}, "本项目中访问 " + o.host
		}
	case opUI:
		if o.app != "" {
			return []Rule{{Kind: RuleApp, Pattern: o.app}}, "本项目中操控 " + o.app
		}
	}
	return []Rule{{Kind: RuleTool, Pattern: o.tool}}, "本项目中使用工具 " + o.tool
}

func (o operation) suggestCmd(mode types.PermissionMode) ([]Rule, string) {
	if o.cmd.Class > o.maxSegClass() || o.cmd.Class == CmdCatastrophic {
		return nil, ""
	}
	var rules []Rule
	var labels []string
	seen := map[string]bool{}
	for _, seg := range o.needSegments(mode) {
		prefix := SegmentPrefix(seg.Words, seg.Class)
		if prefix == nil {
			return nil, "" // 含运行期展开的段不可记住
		}
		pat := JoinPattern(prefix)
		if seen[pat] {
			continue
		}
		seen[pat] = true
		rules = append(rules, Rule{Kind: RuleCmd, Pattern: pat})
		labels = append(labels, "`"+pat+"`")
	}
	if len(rules) == 0 {
		return nil, ""
	}
	return rules, "本项目中 " + strings.Join(labels, "、") + " 开头的命令"
}

func (o operation) suggestDirs(mode types.PermissionMode) ([]Rule, string) {
	need := o.outside
	if mode == types.ModeDefault {
		need = o.targets
	}
	var rules []Rule
	var labels []string
	seen := map[string]bool{}
	for _, t := range need {
		if t == "" {
			return nil, ""
		}
		dir := filepath.Dir(t)
		if seen[dir] {
			continue
		}
		seen[dir] = true
		rules = append(rules, Rule{Kind: RuleDir, Pattern: dir})
		labels = append(labels, dir)
	}
	if len(rules) == 0 {
		return nil, ""
	}
	return rules, "本项目中写入目录 " + strings.Join(labels, "、")
}
