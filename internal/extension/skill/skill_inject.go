package skill

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/extension/pluginspec"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// InjectionTrust 技能动态注入的审阅信任（与 hooks 共用 hook_trust 表，键 "skill:<技能名>"）。
type InjectionTrust interface {
	ListHookTrust(ctx context.Context) (map[string]string, error)
}

// SkillInjector 执行技能正文中的 !`cmd“ / ```! 动态注入（Claude 技能语义，ADR-0103 决策五）。
// 安全模型与插件 hook 一致：命令集合按哈希审阅信任后才执行；事件级 PolicyGate；Rust 沙箱封装
// 执行（封装失败拒绝）；输出与正文一样按技能来源信任等级计污点。
type SkillInjector struct {
	Wrapper  sandbox.ArgvWrapper
	Policy   protocol.PolicyGate
	Trust    InjectionTrust
	Disabled bool // 对应 Claude disableSkillShellExecution：命令替换为提示文本，不执行
}

const injectionDisabledText = "[shell command execution disabled by policy]"

// InjectionTrustKey 技能动态注入的信任键。
func InjectionTrustKey(skillName string) string { return "skill:" + skillName }

// injectionPlan 模板上的注入点（信任按模板命令计算，不随调用参数变化）与渲染后的待执行命令。
type injectionPlan struct {
	points   []pluginspec.Injection
	commands []string
	consumed bool
}

func planInjections(template string, in pluginspec.RenderInput) injectionPlan {
	p := injectionPlan{points: pluginspec.FindInjections(template)}
	for _, pt := range p.points {
		cmd, used := pluginspec.RenderCommand(pt.Command, in)
		p.commands = append(p.commands, cmd)
		p.consumed = p.consumed || used
	}
	return p
}

// run 执行全部注入命令并返回输出；任一命令失败即中止整次技能调用（Claude 语义）。
func (i *SkillInjector) run(ctx context.Context, meta *types.SkillMeta, plan injectionPlan) ([]string, error) {
	outputs := make([]string, len(plan.points))
	if i == nil || i.Disabled {
		for k := range outputs {
			outputs[k] = injectionDisabledText
		}
		return outputs, nil
	}
	if err := i.authorize(ctx, meta, plan); err != nil {
		return nil, err
	}
	for k, cmd := range plan.commands {
		res, err := sandbox.RunStdio(ctx, i.Wrapper, sandbox.StdioRequest{CallerType: protocol.CallerSkill, Command: cmd,
			WorkDir: meta.SkillDir, AllowedPaths: nonEmptyPaths(meta.SkillDir), Timeout: 2 * time.Minute})
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeOf(err), "skill injection", err)
		}
		out := strings.TrimRight(string(res.Stdout)+string(res.Stderr), "\n")
		searchMiss := res.ExitCode == 1 && isSearchCommand(cmd)
		if res.TimedOut || (res.ExitCode != 0 && !searchMiss) {
			return nil, apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("Shell command failed for pattern %q", plan.points[k].Command))
		}
		outputs[k] = out
	}
	return outputs, nil
}

func (i *SkillInjector) authorize(ctx context.Context, meta *types.SkillMeta, plan injectionPlan) error {
	if i.Policy == nil || i.Trust == nil {
		return apperr.New(apperr.CodeForbidden, "skill injection: policy or trust store not configured (fail-closed)")
	}
	trust, err := i.Trust.ListHookTrust(ctx)
	if err != nil {
		return apperr.Wrap(apperr.CodeOf(err), "skill injection: trust", err)
	}
	if trust[InjectionTrustKey(meta.Name)] != pluginspec.InjectionDigest(plan.points) {
		return apperr.New(apperr.CodeForbidden, fmt.Sprintf("skill %s contains shell commands pending review", meta.Name))
	}
	ok, err := i.Policy.IsAuthorized(ctx, "agent", "skill_inject", meta.Name, map[string]any{"trust_tier": int(meta.Trust)})
	if err != nil || !ok {
		return apperr.New(apperr.CodeForbidden, "skill injection denied by policy")
	}
	return nil
}

// isSearchCommand 搜索/比较类命令退出码 1 表示"无匹配/有差异"，属正常结果（Claude 规则）。
func isSearchCommand(cmd string) bool {
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return false
	}
	switch fields[0] {
	case "grep", "egrep", "fgrep", "rg", "diff", "cmp", "test", "[":
		return true
	}
	return false
}

func nonEmptyPaths(vals ...string) []string {
	var out []string
	for _, v := range vals {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}
