package codeact

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// WithStateDir 注入 REPL 状态持久化的根目录（GD-4-002）。
// 空值时 StatefulSession 请求静默降级为普通一次性执行（不报错，仅不生效），
// 与其余可选依赖组件（reviewer/hitlGateway 等）的降级策略保持一致。
func WithStateDir(dir string) CodeActOption {
	return func(c *CodeAct) {
		c.stateDir = dir
	}
}

// codeactStateSubdir 状态快照文件在 stateDir 下的固定子目录名。
const codeactStateSubdir = "codeact_repl_state"

// buildExecutableScript 返回实际写入沙箱临时文件、真正执行的脚本文本。
//
// GD-4-002 设计取舍：CodeAct 按 M07 §7.4 安全约束，每次调用天生是全新临时容器
// 进程；本次不引入常驻 Jupyter Kernel / 长连接进程 —— 那需要独立的进程生命周期
// 管理、内存占用核算、Tier 分级门控，08-设计增强-待办清单.md 已明确建议将其作为
// 独立 Feature 立项评审，此处不做臆测式重实现。转而采用边界更清晰的轻量方案：
// 调用方在请求中显式声明 StatefulSession=true 时，在真正执行的脚本首尾注入固定的
// 状态快照加载/保存样板代码 —— 每次调用仍是完全独立的一次性沙箱进程（沙箱边界、
// 资源配额、L0/L1/L2 安全审查范围均不变，审查作用于原始 req.Code，早于本函数），
// 只是 Python 全局命名空间（含 pandas/numpy 等常见数据分析场景对象）/ Bash 变量
// 通过 pickle / declare -p 快照文件在同一 SessionID 的多次调用间显式传递，从而在
// 不改变任何既有安全属性的前提下解决"复杂多步数据分析任务被迫反复重建上下文"的
// 原始诉求。
//
// 局限（MVP 范围内有意保留，非 bug）：pickle 无法序列化文件句柄/线程/DB 连接等
// 运行时对象，序列化失败的变量会被静默跳过（下次调用中不存在，而不是报错中断）；
// Bash 快照基于全量 declare -p（含沙箱环境自身变量），未做精确 diff。
func (ca *CodeAct) buildExecutableScript(req protocol.CodeActRequest) (string, error) {
	if !req.StatefulSession || req.SessionID == "" || ca.stateDir == "" {
		return req.Code, nil
	}
	if req.Language != "python" && req.Language != "bash" {
		return req.Code, nil
	}

	stateFile, err := ca.sessionStateFile(req.Language, req.SessionID)
	if err != nil {
		return "", err
	}

	if req.Language == "bash" {
		return wrapBashStateful(stateFile, req.Code), nil
	}
	return wrapPythonStateful(stateFile, req.Code), nil
}

// sessionStateFile 计算并确保存在 SessionID 对应的状态文件路径。
// SessionID 来自调用方（LLM tool-call 参数 / HTTP 请求体），视为不可信输入，用
// filepath.Base(filepath.Clean(...)) 净化防路径穿越（与 vfs.WorkspaceManager 现有
// taskID 净化方式一致，见 workspace_manager_test.go）。
func (ca *CodeAct) sessionStateFile(language, sessionID string) (string, error) {
	safeID := filepath.Base(filepath.Clean(sessionID))
	if safeID == "" || safeID == "." || safeID == string(filepath.Separator) {
		return "", apperr.New(apperr.CodeInvalidInput, "code_act: invalid session_id for stateful execution")
	}
	dir := filepath.Join(ca.stateDir, codeactStateSubdir)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "code_act: create REPL state dir", err)
	}
	ext := ".pkl"
	if language == "bash" {
		ext = ".env"
	}
	return filepath.Join(dir, safeID+ext), nil
}

// wrapPythonStateful 在用户代码前后注入固定的 pickle 快照加载/保存样板：
// 加载阶段读取历史状态并 update 进 globals()；保存阶段遍历新的 globals()，
// 逐项尝试 pickle.dumps 探测是否可序列化，不可序列化的值静默跳过。
func wrapPythonStateful(stateFile, code string) string {
	q := pyQuote(stateFile)
	header := fmt.Sprintf(`import os as __ca_os, pickle as __ca_pickle
__CA_STATE_FILE__ = %s
if __ca_os.path.exists(__CA_STATE_FILE__):
    with open(__CA_STATE_FILE__, "rb") as __ca_f:
        try:
            globals().update(__ca_pickle.load(__ca_f))
        except Exception:
            pass
`, q)

	footer := `
def __ca_save_state__():
    __ca_state = {}
    for __ca_k, __ca_v in list(globals().items()):
        if __ca_k.startswith("_"):
            continue
        try:
            __ca_pickle.dumps(__ca_v)
        except Exception:
            continue
        __ca_state[__ca_k] = __ca_v
    with open(__CA_STATE_FILE__, "wb") as __ca_f:
        __ca_pickle.dump(__ca_state, __ca_f)
__ca_save_state__()
`
	return header + "\n" + code + "\n" + footer
}

// wrapBashStateful 在用户代码前后注入固定的变量快照 source/dump 样板。
func wrapBashStateful(stateFile, code string) string {
	q := shQuote(stateFile)
	header := "[ -f " + q + " ] && source " + q + "\n"
	footer := "\ndeclare -p > " + q + " 2>/dev/null\n"
	return header + "\n" + code + "\n" + footer
}

// pyQuote 生成安全的 Python 单引号字符串字面量。stateFile 已经过
// sessionStateFile 的路径净化，此处为防御性转义，不假设其中一定不含特殊字符。
func pyQuote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `'`, `\'`)
	return "'" + s + "'"
}

// shQuote 生成安全的 POSIX shell 双引号字符串，同上为防御性转义。
func shQuote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, `$`, `\$`)
	s = strings.ReplaceAll(s, "`", "\\`")
	return `"` + s + `"`
}
