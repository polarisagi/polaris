// Package classifier — CommandRiskClassifier
//
// 命令风险静态分级器：对 bash/run_command 工具接收的 shell 命令进行四级风险评估。
//
// 四级定义：
//   - RiskSafe  直接执行，写审计日志（ls / cat / echo / grep 等只读命令）
//   - RiskWarn  执行 + 强化审计（git commit / mv / kill 等有副作用但可逆的命令）
//   - RiskHITL  暂停等待人工审批（rm -rf / pip install / curl / sudo 等高风险操作）
//   - RiskDeny  直接拒绝（fork bomb / 写 /dev/zero / 禁用防火墙等不可逆高危操作）
//
// 设计原则：
//   - 静态正则规则为主，零外部依赖，微秒级延迟（不阻塞 Agent 热路径）
//   - fail-open：规则未命中 → RiskSafe（Agent 能力优先，通过审计补偿）
//   - HITL 全流程暂停由调用方（tool/builtin）负责；此包只返回 Verdict
//   - 规则按 DENY → HITL → WARN 顺序匹配（高风险优先）
//
// 架构文档: docs/arch/M07-Tool-Action-Layer.md §安全审核层

package classifier

import (
	"regexp"
	"strings"
)

// RiskLevel 命令风险等级（数值越大风险越高）。
type RiskLevel int

const (
	RiskSafe RiskLevel = iota // 直接执行
	RiskWarn                  // 执行 + 强化审计
	RiskHITL                  // 暂停等待人工审批
	RiskDeny                  // 直接拒绝
)

// String 返回等级的可读名称。
func (r RiskLevel) String() string {
	switch r {
	case RiskSafe:
		return "SAFE"
	case RiskWarn:
		return "WARN"
	case RiskHITL:
		return "HITL"
	case RiskDeny:
		return "DENY"
	default:
		return "UNKNOWN"
	}
}

// Verdict 分级结果。
type Verdict struct {
	Level   RiskLevel
	Reason  string // 触发规则的人读描述
	Pattern string // 匹配的正则模式（调试/审计用）
}

// rule 单条静态规则。
type rule struct {
	level   RiskLevel
	reason  string
	pattern *regexp.Regexp
}

// CommandRiskClassifier 无状态静态规则分级器，可安全并发使用。
type CommandRiskClassifier struct {
	rules []rule
}

// Default 返回内置默认实例（包级单例，懒初始化）。
// 调用方可直接使用 classifier.Default().Classify(cmd)，无需手动构造。
var Default = func() func() *CommandRiskClassifier {
	var instance *CommandRiskClassifier
	return func() *CommandRiskClassifier {
		if instance == nil {
			instance = New(defaultRules())
		}
		return instance
	}
}()

// New 用给定规则集构造分级器。规则按传入顺序匹配（通常 DENY → HITL → WARN）。
func New(rules []Rule) *CommandRiskClassifier {
	compiled := make([]rule, 0, len(rules))
	for _, r := range rules {
		re, err := regexp.Compile(r.Pattern)
		if err != nil {
			// 规则编译失败不应中断启动，记录跳过即可
			continue
		}
		compiled = append(compiled, rule{
			level:   r.Level,
			reason:  r.Reason,
			pattern: re,
		})
	}
	return &CommandRiskClassifier{rules: compiled}
}

// Rule 规则定义（用于 New() 参数和测试注入）。
type Rule struct {
	Level   RiskLevel
	Reason  string
	Pattern string // Go regexp 语法
}

// Classify 对命令字符串进行风险分级，返回最高风险等级的 Verdict。
// 命令会被规范化（去首尾空白、折叠连续空格），提高规则匹配率。
// 若无规则命中，返回 RiskSafe（fail-open 策略）。
func (c *CommandRiskClassifier) Classify(command string) Verdict {
	normalized := normalizeCommand(command)

	// 按规则顺序（DENY → HITL → WARN）匹配，遇到 DENY 立即返回。
	best := Verdict{Level: RiskSafe, Reason: "no matching rule", Pattern: ""}
	for _, r := range c.rules {
		if r.pattern.MatchString(normalized) {
			if r.level > best.Level {
				best = Verdict{
					Level:   r.level,
					Reason:  r.reason,
					Pattern: r.pattern.String(),
				}
			}
			// DENY 是最高级别，无需继续匹配
			if best.Level == RiskDeny {
				return best
			}
		}
	}
	return best
}

// normalizeCommand 对命令字符串做轻量规范化，降低规则被简单变形绕过的风险。
// 不做深度解析（防止规范化本身引入 bug），仅处理常见变体。
func normalizeCommand(cmd string) string {
	// 去首尾空白
	cmd = strings.TrimSpace(cmd)
	// 折叠多余空格（保留换行以便多行命令匹配）
	reSpaces := regexp.MustCompile(`[ \t]+`)
	cmd = reSpaces.ReplaceAllString(cmd, " ")
	return cmd
}
