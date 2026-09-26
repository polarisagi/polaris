// Package pluginspec 是 OpenAI / Anthropic 双标准扩展包的唯一解析器（ADR-0103）。
//
// 只做纯解析与校验：读文件系统、产出归一化模型与诊断，不写 DB、不起进程、不做授权。
// 安装器（lifecycle）、市场同步（marketplace）与 gateway 均经本包读取插件与技能，
// 不得各自再实现清单解析——两份解析器必然随标准演进漂移（ADR-0103 背景：
// 审计时仓内曾有两套互不一致的插件清单解析）。
package pluginspec

import "fmt"

// Severity 诊断级别。Error 表示该组件未加载；Warning 表示已加载但不符合某条规范。
type Severity string

const (
	SeverityError   Severity = "error"
	SeverityWarning Severity = "warning"
)

// Diagnostic 单条解析诊断，逐组件隔离：一个组件出错不影响其他组件
// （agent-plugins 1.0 一致性条款 5）。Rule 为规范条款标识，供 UI 链接与测试断言。
type Diagnostic struct {
	Severity  Severity `json:"severity"`
	Component string   `json:"component"` // manifest / skill / command / agent / hooks / mcp / app / user_config / channel / dependency
	Path      string   `json:"path,omitempty"`
	Rule      string   `json:"rule"`
	Message   string   `json:"message"`
}

func (d Diagnostic) String() string {
	return fmt.Sprintf("[%s] %s %s (%s): %s", d.Severity, d.Component, d.Path, d.Rule, d.Message)
}

// diagnostics 收集器；零值可用。
type diagnostics []Diagnostic

func (ds *diagnostics) errorf(component, path, rule, format string, args ...any) {
	*ds = append(*ds, Diagnostic{SeverityError, component, path, rule, fmt.Sprintf(format, args...)})
}

func (ds *diagnostics) warnf(component, path, rule, format string, args ...any) {
	*ds = append(*ds, Diagnostic{SeverityWarning, component, path, rule, fmt.Sprintf(format, args...)})
}
