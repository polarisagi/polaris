package hook

import (
	"context"
	"regexp"
	"runtime"
	"strings"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/security/classifier"
	"github.com/polarisagi/polaris/pkg/apperr"
)

var hookVarPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)?)\}`)

// expandHookVars 展开插件路径变量；allowUserConfig=false 时（shell 形式）引用 ${user_config.*}
// 视为错误：值会被 shell 再解析，等于把用户配置当代码执行（Claude 同样拒绝）。
func expandHookVars(s string, src *Source, cwd string, allowUserConfig bool) (string, error) {
	var bad string
	out := hookVarPattern.ReplaceAllStringFunc(s, func(tok string) string {
		name := hookVarPattern.FindStringSubmatch(tok)[1]
		switch name {
		case "CLAUDE_PLUGIN_ROOT", "PLUGIN_ROOT":
			return src.PluginRoot
		case "CLAUDE_PLUGIN_DATA", "PLUGIN_DATA":
			return src.PluginData
		case "CLAUDE_PROJECT_DIR":
			return cwd
		}
		if key, ok := strings.CutPrefix(name, "user_config."); ok {
			if !allowUserConfig {
				bad = name
				return tok
			}
			return src.Options[key]
		}
		return tok // 其余 ${VAR} 交给 shell 按进程环境解析
	})
	if bad != "" {
		return "", apperr.New(apperr.CodeInvalidInput, "hook: shell-form command must not reference ${"+bad+"}; use exec form (args) or CLAUDE_PLUGIN_OPTION_* env")
	}
	return out, nil
}

// hookEnv 两家约定导出给 hook 进程的变量。
func hookEnv(src *Source, cwd string) []string {
	env := []string{"CLAUDE_PROJECT_DIR=" + cwd}
	if src.PluginRoot != "" {
		env = append(env, "CLAUDE_PLUGIN_ROOT="+src.PluginRoot, "PLUGIN_ROOT="+src.PluginRoot)
	}
	if src.PluginData != "" {
		env = append(env, "CLAUDE_PLUGIN_DATA="+src.PluginData, "PLUGIN_DATA="+src.PluginData)
	}
	for k, v := range src.Options {
		env = append(env, "CLAUDE_PLUGIN_OPTION_"+strings.ToUpper(k)+"="+v)
	}
	return env
}

func (r *Runner) runCommand(ctx context.Context, bh boundHandler, in Input, payload []byte) handlerResult {
	h, src := bh.Handler, bh.Source
	command := h.Command
	if runtime.GOOS == "windows" && h.CommandWindows != "" {
		command = h.CommandWindows
	}
	cwd := in.Cwd
	if cwd == "" {
		cwd = src.PluginRoot
	}
	req := sandbox.StdioRequest{CallerType: protocol.CallerHook, Stdin: payload, Env: hookEnv(src, cwd),
		WorkDir: cwd, AllowedPaths: nonEmpty(src.PluginRoot, src.PluginData, cwd), Timeout: h.Timeout(),
		// 信任绑定到定义哈希：用户已审阅的正是这条命令，网络访问是其声明语义的一部分。
		AllowNet: true}
	var err error
	if len(h.Args) > 0 {
		if req.ExecPath, err = expandHookVars(command, src, cwd, false); err != nil {
			return handlerResult{Err: err}
		}
		for _, a := range h.Args {
			v, _ := expandHookVars(a, src, cwd, true)
			req.Args = append(req.Args, v)
		}
	} else if req.Command, err = expandHookVars(command, src, cwd, false); err != nil {
		return handlerResult{Err: err}
	}
	if v := r.risk.Classify(strings.TrimSpace(req.Command + " " + req.ExecPath + " " + strings.Join(req.Args, " "))); v.Level == classifier.RiskDeny {
		return handlerResult{Err: apperr.New(apperr.CodeForbidden, "hook: command denied by risk classifier: "+v.Reason)}
	}
	res, err := sandbox.RunStdio(ctx, r.deps.Wrapper, req)
	if err != nil {
		return handlerResult{Err: err}
	}
	if res.TimedOut {
		return handlerResult{Err: apperr.New(apperr.CodeTimeout, "hook: command timed out")}
	}
	return handlerResult{ExitCode: res.ExitCode, Stdout: res.Stdout, Stderr: res.Stderr}
}

func nonEmpty(vals ...string) []string {
	var out []string
	for _, v := range vals {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}
