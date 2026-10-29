package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"

	"github.com/polarisagi/polaris/pkg/apperr"
)

var envRefPattern = regexp.MustCompile(`\$\{?([A-Za-z_][A-Za-z0-9_]*)\}?`)

// runHTTP POST 输入 JSON；2xx 响应体按 stdout 语义解析，非 2xx 为非阻断错误（两家一致）。
// 出站经 SafeDialer（SSRF 防护，XR-06）。
func (r *Runner) runHTTP(ctx context.Context, bh boundHandler, payload []byte) handlerResult {
	if !r.deps.HTTPClient.IsSafe() {
		return handlerResult{Err: apperr.New(apperr.CodeForbidden, "hook: safe http client not configured (fail-closed)")}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, bh.Handler.URL, bytes.NewReader(payload))
	if err != nil {
		return handlerResult{Err: apperr.Wrap(apperr.CodeInvalidInput, "hook: http request", err)}
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range bh.Handler.Headers {
		req.Header.Set(k, expandAllowedEnv(v, bh))
	}
	resp, err := r.deps.HTTPClient.Do(req)
	if err != nil {
		return handlerResult{Err: apperr.Wrap(apperr.CodeNetworkUnavailable, "hook: http call", err)}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return handlerResult{Err: apperr.Wrap(apperr.CodeNetworkUnavailable, "hook: http read", err)}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return handlerResult{Err: apperr.New(apperr.CodeInternal, fmt.Sprintf("hook: http status %d", resp.StatusCode))}
	}
	return handlerResult{Stdout: body}
}

// expandAllowedEnv 仅展开 allowedEnvVars 显式列出的变量（Claude 规则）；插件选项
// CLAUDE_PLUGIN_OPTION_* 取自 userConfig，其余取本机环境。未列出的引用保持原文，防止
// 头部模板把任意宿主变量带出。
func expandAllowedEnv(v string, bh boundHandler) string {
	allowed := map[string]bool{}
	for _, k := range bh.Handler.AllowedEnvVars {
		allowed[k] = true
	}
	return envRefPattern.ReplaceAllStringFunc(v, func(tok string) string {
		name := envRefPattern.FindStringSubmatch(tok)[1]
		if !allowed[name] {
			return tok
		}
		if key, ok := strings.CutPrefix(name, "CLAUDE_PLUGIN_OPTION_"); ok {
			for k, val := range bh.Source.Options {
				if strings.EqualFold(k, key) {
					return val
				}
			}
			return ""
		}
		return os.Getenv(name)
	})
}

// runMCPTool 调用已连接 MCP 服务器的工具；input 模板中的 ${a.b} 按输入 JSON 路径替换。
func (r *Runner) runMCPTool(ctx context.Context, bh boundHandler, payload []byte) handlerResult {
	if r.deps.MCP == nil {
		return handlerResult{Err: apperr.New(apperr.CodeInternal, "hook: mcp tool caller not configured")}
	}
	var in map[string]any
	if err := json.Unmarshal(payload, &in); err != nil {
		return handlerResult{Err: apperr.Wrap(apperr.CodeInternal, "hook: decode input", err)}
	}
	args := map[string]any{}
	for k, v := range bh.Handler.Input {
		args[k] = substituteTemplate(v, in)
	}
	out, err := r.deps.MCP.CallHookTool(ctx, bh.Handler.Server, bh.Handler.Tool, args)
	if err != nil {
		return handlerResult{Err: apperr.Wrap(apperr.CodeOf(err), "hook: mcp tool", err)}
	}
	return handlerResult{Stdout: []byte(out)}
}

var templatePattern = regexp.MustCompile(`\$\{([A-Za-z0-9_.]+)\}`)

func substituteTemplate(v any, in map[string]any) any {
	s, ok := v.(string)
	if !ok {
		return v
	}
	return templatePattern.ReplaceAllStringFunc(s, func(tok string) string {
		cur := any(in)
		for _, part := range strings.Split(templatePattern.FindStringSubmatch(tok)[1], ".") {
			m, ok := cur.(map[string]any)
			if !ok {
				return ""
			}
			cur = m[part]
		}
		if cur == nil {
			return ""
		}
		if str, ok := cur.(string); ok {
			return str
		}
		b, _ := json.Marshal(cur)
		return string(b)
	})
}

// runPrompt prompt / agent 处理器：$ARGUMENTS 替换为输入 JSON；模型须返回 {"ok": bool, "reason": ...}，
// ok=false 映射为阻断（等价 exit 2），无法解析的输出按非阻断错误处理。
func (r *Runner) runPrompt(ctx context.Context, bh boundHandler, payload []byte) handlerResult {
	if r.deps.Prompt == nil {
		return handlerResult{Err: apperr.New(apperr.CodeInternal, "hook: prompt evaluator not configured")}
	}
	prompt := strings.ReplaceAll(bh.Handler.Prompt, "$ARGUMENTS", string(payload))
	if !strings.Contains(bh.Handler.Prompt, "$ARGUMENTS") {
		prompt += "\n\nHook input:\n" + string(payload)
	}
	raw, err := r.deps.Prompt.EvaluateHookPrompt(ctx, prompt, bh.Handler.Model, bh.Handler.Type == TypeAgent)
	if err != nil {
		return handlerResult{Err: apperr.Wrap(apperr.CodeOf(err), "hook: prompt evaluation", err)}
	}
	var verdict struct {
		OK     *bool  `json:"ok"`
		Reason string `json:"reason"`
	}
	text := strings.TrimSpace(raw)
	if i, j := strings.Index(text, "{"), strings.LastIndex(text, "}"); i >= 0 && j > i {
		text = text[i : j+1]
	}
	if err := json.Unmarshal([]byte(text), &verdict); err != nil || verdict.OK == nil {
		return handlerResult{Err: apperr.New(apperr.CodeInternal, "hook: prompt hook returned no {ok} verdict")}
	}
	if !*verdict.OK {
		return handlerResult{ExitCode: 2, Stderr: []byte(firstNonBlank(verdict.Reason, "rejected by prompt hook"))}
	}
	return handlerResult{}
}
