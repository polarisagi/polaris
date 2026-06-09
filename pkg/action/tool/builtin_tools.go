package tool

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"fmt"

	perrors "github.com/polarisagi/polaris/internal/errors"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sysenv"
	"github.com/polarisagi/polaris/pkg/action"
)

// RegisterBuiltinTools 注册所有内置工具到 sandbox 与 registry，并绑定 InProcessSandbox 为执行器。
// 工具元数据（名称/描述/Schema）从 builtin/<name>/tool.yaml + schema.json 文件加载，
// 实现函数在本文件中定义。安全约束由平台原生沙箱 + 路径白名单双重保证。
// 调用方式: 系统启动时调用一次（非线程安全）。
func RegisterBuiltinTools(
	sandbox *action.InProcessSandbox,
	toolReg *InMemoryToolRegistry,
	allowedPaths []string, // 文件系统路径白名单（read_file/list_dir/write_file 均受限）
	dialer protocol.SafeDialer,
	sandboxEnabled bool,      // 是否启用平台原生进程沙箱
	netPolicy NetworkPolicy,  // bash/run_command 网络访问策略
	bwrapPath string,         // Linux: bwrap 路径（空=自动查找）
) error {
	// 元数据与实现绑定表：name → InProcessFn
	// 元数据从 builtin/<name>/tool.yaml + schema.json 加载，不再硬编码在此处。
	defs := []struct {
		name string
		fn   action.InProcessFn
	}{
		{"read_file", makeReadFileFn(allowedPaths)},
		{"list_dir", makeListDirFn(allowedPaths)},
		{"write_file", makeWriteFileFn(allowedPaths)},
		{"fetch_url", makeFetchURLFn(dialer)},
		{"bash", makeBashFn(allowedPaths, sandboxEnabled, netPolicy, bwrapPath)},
		{"run_command", makeRunCommandFn(allowedPaths, sandboxEnabled, netPolicy, bwrapPath)},
		{"get_datetime", getDatetimeFn},
		{"csv_parse", csvParseFn},
		{"diff_text", diffTextFn},
		{"tts_edge", ExecuteEdgeTTS},
		{"video_analysis", ExecuteVideoAnalysis},
		{"sys_probe", sysProbeFn},
		{"str_replace_editor", makeStrReplaceEditorFn(allowedPaths)},
		{"read_tool_ref", makeReadToolRefFn()},
	}

	for _, d := range defs {
		meta, err := LoadBuiltinToolMeta(d.name)
		if err != nil {
			return perrors.Wrap(perrors.CodeInternal,
				fmt.Sprintf("builtin_tools: load meta for %q", d.name), err)
		}
		sandbox.Register(meta.Name, d.fn)
		if err := toolReg.Register(meta); err != nil {
			return perrors.Wrap(perrors.CodeInternal,
				fmt.Sprintf("builtin_tools: register %q", d.name), err)
		}
	}

	// 将 InProcessSandbox 绑定为工具注册表的真实执行器，替代 stub
	toolReg.SetSandbox(sandbox)
	return nil
}

// ── 以下为纯实现函数，不含任何元数据 ─────────────────────────────────────────

// ─── read_file ────────────────────────────────────────────────────────────────

type readFileArgs struct {
	Path string `json:"path"`
}

func makeReadFileFn(allowedPaths []string) action.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		var args readFileArgs
		if err := json.Unmarshal(input, &args); err != nil {
			return nil, perrors.Wrap(perrors.CodeInternal, "read_file: invalid args", err)
		}
		if err := checkAllowedPath(args.Path, allowedPaths); err != nil {
			return nil, err
		}

		data, err := os.ReadFile(filepath.Clean(args.Path))
		if err != nil {
			return nil, perrors.Wrap(perrors.CodeInternal, "read_file", err)
		}
		return data, nil
	}
}

// ─── read_tool_ref ────────────────────────────────────────────────────────────

type readToolRefArgs struct {
	ID string `json:"id"`
}

func makeReadToolRefFn() action.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		var args readToolRefArgs
		if err := json.Unmarshal(input, &args); err != nil {
			return nil, perrors.Wrap(perrors.CodeInternal, "read_tool_ref: invalid args", err)
		}
		if args.ID == "" {
			return nil, perrors.New(perrors.CodeInternal, "read_tool_ref: id is required")
		}

		home, err := os.UserHomeDir()
		if err != nil {
			return nil, perrors.Wrap(perrors.CodeInternal, "read_tool_ref: home dir not found", err)
		}

		// Security: prevent path traversal
		cleanID := filepath.Base(args.ID)
		path := filepath.Join(home, ".polarisagi", "polaris", "data", "tool_refs", cleanID+".log")

		data, err := os.ReadFile(path)
		if err != nil {
			return nil, perrors.Wrap(perrors.CodeInternal, "read_tool_ref: file read error", err)
		}
		return data, nil
	}
}

// ─── list_dir ────────────────────────────────────────────────────────────────

type listDirArgs struct {
	Path string `json:"path"`
}

type listDirResult struct {
	Entries []dirEntry `json:"entries"`
}

type dirEntry struct {
	Name  string `json:"name"`
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size_bytes"`
}

func makeListDirFn(allowedPaths []string) action.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		var args listDirArgs
		if err := json.Unmarshal(input, &args); err != nil {
			return nil, perrors.Wrap(perrors.CodeInternal, "list_dir: invalid args", err)
		}
		if err := checkAllowedPath(args.Path, allowedPaths); err != nil {
			return nil, err
		}

		entries, err := os.ReadDir(filepath.Clean(args.Path))
		if err != nil {
			return nil, perrors.Wrap(perrors.CodeInternal, "list_dir", err)
		}

		result := listDirResult{Entries: make([]dirEntry, 0, len(entries))}
		for _, e := range entries {
			info, _ := e.Info()
			var sz int64
			if info != nil {
				sz = info.Size()
			}
			result.Entries = append(result.Entries, dirEntry{
				Name:  e.Name(),
				IsDir: e.IsDir(),
				Size:  sz,
			})
		}
		return json.Marshal(result)
	}
}

// ─── write_file ───────────────────────────────────────────────────────────────

type writeFileArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Append  bool   `json:"append"`
}

func makeWriteFileFn(allowedPaths []string) action.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		var args writeFileArgs
		if err := json.Unmarshal(input, &args); err != nil {
			return nil, perrors.Wrap(perrors.CodeInternal, "write_file: invalid args", err)
		}
		if err := checkAllowedPath(args.Path, allowedPaths); err != nil {
			return nil, err
		}

		flag := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
		if args.Append {
			flag = os.O_WRONLY | os.O_CREATE | os.O_APPEND
		}

		f, err := os.OpenFile(filepath.Clean(args.Path), flag, 0o600)
		if err != nil {
			return nil, perrors.Wrap(perrors.CodeInternal, "write_file", err)
		}
		defer f.Close()

		if _, err := f.WriteString(args.Content); err != nil {
			return nil, perrors.Wrap(perrors.CodeInternal, "write_file: write error", err)
		}
		return []byte(`{"written":true}`), nil
	}
}

// ─── fetch_url ────────────────────────────────────────────────────────────────

type fetchURLArgs struct {
	URL string `json:"url"`
}

// makeFetchURLFn 返回 fetch_url 工具函数。
func makeFetchURLFn(dialer protocol.SafeDialer) action.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		if dialer == nil {
			return nil, perrors.New(perrors.CodeInternal, "fetch_url: SafeDialer is required (XR-06 violation prevented)")
		}

		client := &http.Client{
			Transport: &http.Transport{
				DialContext: dialer.DialContext,
			},
			Timeout: 30 * time.Second,
		}

		var args fetchURLArgs
		if err := json.Unmarshal(input, &args); err != nil {
			return nil, perrors.Wrap(perrors.CodeInternal, "fetch_url: invalid args", err)
		}
		if args.URL == "" {
			return nil, perrors.New(perrors.CodeInternal, "fetch_url: url is required")
		}

		// SSRF Guard Phase 1: 基础文本正则检查 (SafeDialer 内部会有更严格的解析检查)
		if isPrivateURL(args.URL) {
			return nil, perrors.New(perrors.CodeInternal, fmt.Sprintf("fetch_url: SSRF guard blocked private URL: %s", args.URL))
		}

		req, err := http.NewRequestWithContext(ctx, "GET", args.URL, nil)
		if err != nil {
			return nil, perrors.Wrap(perrors.CodeInternal, "fetch_url: bad request", err)
		}

		// 伪装 User-Agent，避免被简单的爬虫拦截
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")

		resp, err := client.Do(req)
		if err != nil {
			return nil, perrors.Wrap(perrors.CodeInternal, "fetch_url: request failed", err)
		}
		defer resp.Body.Close()

		// 限制读取大小（最大 2MB），防止内存溢出
		bodyReader := io.LimitReader(resp.Body, 2*1024*1024)
		body, err := io.ReadAll(bodyReader)
		if err != nil {
			return nil, perrors.Wrap(perrors.CodeInternal, "fetch_url: read response body failed", err)
		}

		// 如果超出了限制
		truncated := false
		if len(body) == 2*1024*1024 {
			truncated = true
		}

		contentStr := string(body)
		contentType := resp.Header.Get("Content-Type")
		if strings.Contains(contentType, "text/html") {
			// MVP 阶段：简单的正则清洗 HTML 标签
			tagRe := regexp.MustCompile(`<[^>]*>`)
			spaceRe := regexp.MustCompile(`\s+`)
			contentStr = tagRe.ReplaceAllString(contentStr, " ")
			contentStr = strings.TrimSpace(spaceRe.ReplaceAllString(contentStr, " "))
		}

		result := map[string]any{
			"url":       args.URL,
			"status":    resp.StatusCode,
			"truncated": truncated,
			"content":   contentStr,
		}
		return json.Marshal(result)
	}
}

// ─── bash ───────────────────────────────────────────────────────────────────────

type bashArgs struct {
	Command string `json:"command"`
}

// baseEnv 返回清理后的最小环境变量集（防止 LLM 通过 env 注入攻击）。
func baseEnv() []string {
	return []string{
		"PATH=/usr/local/bin:/usr/bin:/bin:/sbin:/usr/sbin:/opt/homebrew/bin",
		"HOME=/tmp",
		"TMPDIR=/tmp",
	}
}

func makeBashFn(allowedPaths []string, sandboxEnabled bool, netPolicy NetworkPolicy, bwrapPath string) action.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		var args bashArgs
		if err := json.Unmarshal(input, &args); err != nil {
			return nil, perrors.Wrap(perrors.CodeInternal, "bash: invalid args", err)
		}
		if args.Command == "" {
			return nil, perrors.New(perrors.CodeInternal, "bash: command is required")
		}

		workDir := ""
		if len(allowedPaths) > 0 {
			workDir = allowedPaths[0]
		}

		execCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()

		slog.Warn("native_sandbox: executing bash command",
			"sandbox_enabled", sandboxEnabled,
			"network", netPolicy,
			"cmd", args.Command,
			"dir", workDir)

		var cmd *exec.Cmd
		var err error
		if sandboxEnabled {
			cfg := NativeSandboxCfg{
				Command:       args.Command,
				WorkDir:       workDir,
				AllowedPaths:  allowedPaths,
				NetworkPolicy: netPolicy,
				Env:           baseEnv(),
				BwrapPath:     bwrapPath,
			}
			cmd, err = WrapBashCmd(execCtx, cfg)
			if err != nil {
				return nil, perrors.Wrap(perrors.CodeInternal, "bash: sandbox wrap failed", err)
			}
		} else {
			// 沙箱禁用：仅 env 清理 + workDir + Linux namespace（最后防线）
			cmd = exec.CommandContext(execCtx, "bash", "-c", args.Command)
			cmd.Dir = workDir
			cmd.Env = baseEnv()
			if attrs := action.ContainerSandboxSysProcAttr(); attrs != nil {
				cmd.SysProcAttr = attrs
			}
		}

		outBytes, execErr := cmd.CombinedOutput()
		result := map[string]any{
			"command":          args.Command,
			"output":           string(outBytes),
			"exit_code":        0,
			"sandbox_enabled":  sandboxEnabled,
			"network_policy":   string(netPolicy),
		}
		if execErr != nil {
			result["error"] = execErr.Error()
			var exitErr *exec.ExitError
			if errors.As(execErr, &exitErr) {
				result["exit_code"] = exitErr.ExitCode()
			} else {
				result["exit_code"] = -1
			}
		}
		return json.Marshal(result)
	}
}

// ─── get_datetime ────────────────────────────────────────────────────────────

var getDatetimeFn action.InProcessFn = func(_ context.Context, _ []byte) ([]byte, error) {
	now := time.Now()
	result := map[string]any{
		"utc":      now.UTC().Format(time.RFC3339),
		"local":    now.Format(time.RFC3339),
		"unix":     now.Unix(),
		"timezone": now.Location().String(),
	}
	return json.Marshal(result)
}

// ─── sys_probe ───────────────────────────────────────────────────────────────

var sysProbeFn action.InProcessFn = func(_ context.Context, _ []byte) ([]byte, error) {
	info := sysenv.GetSystemInfo()
	return json.Marshal(info)
}

// ─── csv_parse ────────────────────────────────────────────────────────────────

type csvParseArgs struct {
	CSV string `json:"csv"`
}

var csvParseFn action.InProcessFn = func(_ context.Context, input []byte) ([]byte, error) {
	var args csvParseArgs
	if err := json.Unmarshal(input, &args); err != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, "csv_parse: invalid args", err)
	}
	if args.CSV == "" {
		return nil, perrors.New(perrors.CodeInternal, "csv_parse: csv is required")
	}

	lines := strings.Split(strings.ReplaceAll(args.CSV, "\r\n", "\n"), "\n")
	// 过滤空行
	var nonEmpty []string
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			nonEmpty = append(nonEmpty, l)
		}
	}
	if len(nonEmpty) < 2 {
		return json.Marshal([]map[string]string{})
	}

	// 解析表头
	headers := splitCSVLine(nonEmpty[0])
	rows := make([]map[string]string, 0, len(nonEmpty)-1)
	for _, line := range nonEmpty[1:] {
		cols := splitCSVLine(line)
		row := make(map[string]string, len(headers))
		for i, h := range headers {
			if i < len(cols) {
				row[h] = cols[i]
			} else {
				row[h] = ""
			}
		}
		rows = append(rows, row)
	}
	return json.Marshal(rows)
}

// splitCSVLine 解析单行 CSV（支持双引号转义）。
func splitCSVLine(line string) []string {
	var fields []string
	var cur strings.Builder
	inQuote := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case c == '"' && !inQuote:
			inQuote = true
		case c == '"' && inQuote:
			// 连续两个引号 → 转义单引号
			if i+1 < len(line) && line[i+1] == '"' {
				cur.WriteByte('"')
				i++
			} else {
				inQuote = false
			}
		case c == ',' && !inQuote:
			fields = append(fields, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	fields = append(fields, cur.String())
	return fields
}

// ─── diff_text ────────────────────────────────────────────────────────────────

type diffTextArgs struct {
	Old string `json:"old"`
	New string `json:"new"`
}

var diffTextFn action.InProcessFn = func(_ context.Context, input []byte) ([]byte, error) {
	var args diffTextArgs
	if err := json.Unmarshal(input, &args); err != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, "diff_text: invalid args", err)
	}

	oldLines := strings.Split(args.Old, "\n")
	newLines := strings.Split(args.New, "\n")
	diff := computeUnifiedDiff(oldLines, newLines)

	result := map[string]any{
		"diff":     diff,
		"has_diff": diff != "",
	}
	return json.Marshal(result)
}

// computeUnifiedDiff 生成简化 unified diff（LCS 算法）。
func computeUnifiedDiff(oldLines, newLines []string) string { //nolint:gocyclo
	// LCS 长度表
	m, n := len(oldLines), len(newLines)
	dp := make([][]int, m+1)
	for i := range dp {
		dp[i] = make([]int, n+1)
	}
	for i := 1; i <= m; i++ {
		for j := 1; j <= n; j++ {
			if oldLines[i-1] == newLines[j-1] {
				dp[i][j] = dp[i-1][j-1] + 1
			} else if dp[i-1][j] >= dp[i][j-1] {
				dp[i][j] = dp[i-1][j]
			} else {
				dp[i][j] = dp[i][j-1]
			}
		}
	}

	// 回溯构造差异列表
	type op struct {
		kind byte // ' ' '+' '-'
		line string
	}
	var ops []op
	i, j := m, n
	for i > 0 || j > 0 {
		switch {
		case i > 0 && j > 0 && oldLines[i-1] == newLines[j-1]:
			ops = append(ops, op{' ', oldLines[i-1]})
			i--
			j--
		case j > 0 && (i == 0 || dp[i][j-1] >= dp[i-1][j]):
			ops = append(ops, op{'+', newLines[j-1]})
			j--
		default:
			ops = append(ops, op{'-', oldLines[i-1]})
			i--
		}
	}
	// 反转
	for l, r := 0, len(ops)-1; l < r; l, r = l+1, r-1 {
		ops[l], ops[r] = ops[r], ops[l]
	}

	// 输出有变化的行（带 context=3）
	const ctx = 3
	changed := make([]bool, len(ops))
	for idx, o := range ops {
		if o.kind != ' ' {
			changed[idx] = true
		}
	}

	var sb strings.Builder
	sb.WriteString("--- old\n+++ new\n")
	printed := make([]bool, len(ops))
	for idx := range ops {
		if !changed[idx] {
			continue
		}
		start := max(idx-ctx, 0)
		end := min(idx+ctx+1, len(ops))
		for k := start; k < end; k++ {
			if printed[k] {
				continue
			}
			printed[k] = true
			switch ops[k].kind {
			case '+':
				sb.WriteString("+" + ops[k].line + "\n")
			case '-':
				sb.WriteString("-" + ops[k].line + "\n")
			default:
				sb.WriteString(" " + ops[k].line + "\n")
			}
		}
	}
	return sb.String()
}

// ─── 辅助函数 ─────────────────────────────────────────────────────────────────

// checkAllowedPath 确认 path 在白名单内（防路径穿越）。
// 若白名单为空则拒绝所有访问（fail-closed）。
// 使用 == 或 Separator 前缀双重校验，防止 /allowed-extra 通过 /allowed 白名单。
func checkAllowedPath(path string, allowedPaths []string) error {
	if len(allowedPaths) == 0 {
		return perrors.New(perrors.CodeInternal, "path_guard: no allowed paths configured (fail-closed)")
	}
	clean := filepath.Clean(path)
	for _, allowed := range allowedPaths {
		allowedClean := filepath.Clean(allowed)
		if clean == allowedClean || strings.HasPrefix(clean, allowedClean+string(filepath.Separator)) {
			return nil
		}
	}
	return perrors.New(perrors.CodeInternal, fmt.Sprintf("path_guard: path %q not in allowed paths", path))
}

// isPrivateURL 判断 URL 是否指向私有/内网地址（SSRF Guard 阶段 1）。
func isPrivateURL(rawURL string) bool {
	privatePatterns := []string{
		"localhost", "127.", "10.", "192.168.", "172.16.", "169.254.",
		"::1", "0.0.0.0", "metadata.google", "169.254.169.254",
	}
	lower := strings.ToLower(rawURL)
	for _, p := range privatePatterns {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

// ─── str_replace_editor ──────────────────────────────────────────────────────

type strReplaceEditorArgs struct {
	Command string `json:"command"` // create, str_replace, view, undo_edit
	Path    string `json:"path"`
	OldStr  string `json:"old_str"`
	NewStr  string `json:"new_str"`
}

// 内存中保存最近一次修改的备份 (session 级别应该在外部控制，这里简化为全局 map 作为 MVP)
var undoBuffer = make(map[string]string)

func makeStrReplaceEditorFn(allowedPaths []string) action.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		var args strReplaceEditorArgs
		if err := json.Unmarshal(input, &args); err != nil {
			return nil, perrors.Wrap(perrors.CodeInternal, "str_replace_editor: invalid args", err)
		}
		if err := checkAllowedPath(args.Path, allowedPaths); err != nil {
			return nil, err
		}

		cleanPath := filepath.Clean(args.Path)

		switch args.Command {
		case "create":
			if _, err := os.Stat(cleanPath); err == nil {
				return nil, perrors.New(perrors.CodeInternal, "str_replace_editor: file already exists")
			}
			if err := os.WriteFile(cleanPath, []byte(args.NewStr), 0600); err != nil {
				return nil, perrors.Wrap(perrors.CodeInternal, "str_replace_editor: create failed", err)
			}
			return []byte(`{"status":"created"}`), nil

		case "view":
			data, err := os.ReadFile(cleanPath)
			if err != nil {
				return nil, perrors.Wrap(perrors.CodeInternal, "str_replace_editor: view failed", err)
			}
			return data, nil

		case "str_replace":
			return executeStrReplace(cleanPath, args)

		case "undo_edit":
			oldContent, ok := undoBuffer[cleanPath]
			if !ok {
				return nil, perrors.New(perrors.CodeInternal, "str_replace_editor: no undo history found for this file")
			}
			if err := os.WriteFile(cleanPath, []byte(oldContent), 0600); err != nil {
				return nil, perrors.Wrap(perrors.CodeInternal, "str_replace_editor: undo write failed", err)
			}
			delete(undoBuffer, cleanPath)
			return []byte(`{"status":"undone"}`), nil

		default:
			return nil, perrors.New(perrors.CodeInternal, fmt.Sprintf("str_replace_editor: unknown command %q", args.Command))
		}
	}
}

func executeStrReplace(cleanPath string, args strReplaceEditorArgs) ([]byte, error) {
	data, err := os.ReadFile(cleanPath)
	if err != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, "str_replace_editor: read failed", err)
	}
	content := string(data)

	if args.OldStr == "" {
		return nil, perrors.New(perrors.CodeInternal, "str_replace_editor: old_str cannot be empty")
	}

	count := strings.Count(content, args.OldStr)
	if count == 0 {
		return nil, perrors.New(perrors.CodeInternal, "str_replace_editor: old_str not found in file")
	}
	if count > 1 {
		return nil, perrors.New(perrors.CodeInternal, "str_replace_editor: old_str is not unique, matched multiple times. Please provide more context in old_str.")
	}

	// 备份到 undoBuffer
	undoBuffer[cleanPath] = content

	newContent := strings.Replace(content, args.OldStr, args.NewStr, 1)
	if err := os.WriteFile(cleanPath, []byte(newContent), 0600); err != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, "str_replace_editor: write failed", err)
	}
	return []byte(`{"status":"replaced"}`), nil
}

// ─── run_command ─────────────────────────────────────────────────────────────

type runCommandArgs struct {
	Command    string `json:"command"`
	WorkingDir string `json:"working_dir"`
	TimeoutS   int    `json:"timeout_s"`
}

func makeRunCommandFn(allowedPaths []string, sandboxEnabled bool, netPolicy NetworkPolicy, bwrapPath string) action.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		var args runCommandArgs
		if err := json.Unmarshal(input, &args); err != nil {
			return nil, perrors.Wrap(perrors.CodeInternal, "run_command: invalid args", err)
		}
		if args.Command == "" {
			return nil, perrors.New(perrors.CodeInternal, "run_command: command is required")
		}

		// 命令前缀白名单（构建工具，不含 bash/sh 等 shell 解释器）
		cmdPrefix := strings.SplitN(strings.TrimSpace(args.Command), " ", 2)[0]
		allowedCmds := map[string]bool{
			"go": true, "cargo": true, "npm": true, "yarn": true, "pnpm": true,
			"make": true, "pytest": true, "tsc": true, "python": true, "python3": true,
			"pip": true, "pip3": true, "node": true, "deno": true, "bun": true,
		}
		if !allowedCmds[cmdPrefix] {
			return nil, perrors.New(perrors.CodeForbidden, fmt.Sprintf("run_command: command %q not in whitelist", cmdPrefix))
		}

		workDir := args.WorkingDir
		if workDir == "" && len(allowedPaths) > 0 {
			workDir = allowedPaths[0]
		}
		if workDir != "" {
			if err := checkAllowedPath(workDir, allowedPaths); err != nil {
				return nil, err
			}
		}

		timeout := time.Duration(args.TimeoutS) * time.Second
		if timeout <= 0 || timeout > 120*time.Second {
			timeout = 30 * time.Second
		}

		execCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()

		slog.Info("run_command: executing",
			"sandbox_enabled", sandboxEnabled,
			"network", netPolicy,
			"cmd", args.Command,
			"dir", workDir)

		env := append(baseEnv(), "GOCACHE=/tmp/gocache", "CARGO_HOME=/tmp/cargo", "npm_config_cache=/tmp/npm")

		var cmd *exec.Cmd
		var cmdErr error
		if sandboxEnabled {
			cfg := NativeSandboxCfg{
				Command:       args.Command,
				WorkDir:       workDir,
				AllowedPaths:  allowedPaths,
				NetworkPolicy: netPolicy, // 构建工具通常需要网络（下载依赖），由上层配置控制
				Env:           env,
				BwrapPath:     bwrapPath,
			}
			cmd, cmdErr = WrapBashCmd(execCtx, cfg)
			if cmdErr != nil {
				return nil, perrors.Wrap(perrors.CodeInternal, "run_command: sandbox wrap failed", cmdErr)
			}
		} else {
			cmd = exec.CommandContext(execCtx, "bash", "-c", args.Command)
			cmd.Dir = workDir
			cmd.Env = env
			if attrs := action.ContainerSandboxSysProcAttr(); attrs != nil {
				cmd.SysProcAttr = attrs
			}
		}

		outBytes, execErr := cmd.CombinedOutput()
		result := map[string]any{
			"command":   args.Command,
			"output":    string(outBytes),
			"exit_code": 0,
		}
		if execErr != nil {
			result["error"] = execErr.Error()
			var exitErr *exec.ExitError
			if errors.As(execErr, &exitErr) {
				result["exit_code"] = exitErr.ExitCode()
			} else {
				result["exit_code"] = -1
			}
		}
		return json.Marshal(result)
	}
}
