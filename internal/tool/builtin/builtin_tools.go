package builtin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/polarisagi/polaris/internal/tool"
	toolsb "github.com/polarisagi/polaris/internal/tool/sandbox"

	"github.com/bmatcuk/doublestar/v4"

	_ "modernc.org/sqlite" // data_query 工具的 SQLite 驱动（ADR-0003：纯 Go，无 CGO）

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/security/classifier"
	"github.com/polarisagi/polaris/internal/sysmgr/sysinfo"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// RegisterBuiltinTools 注册所有内置工具到 sandbox 与 registry，并绑定 InProcessSandbox 为执行器。
// 工具元数据（名称/描述/Schema）从 builtin/<name>/tool.yaml + schema.json 文件加载，
// 实现函数在本文件中定义。安全约束由平台原生沙箱 + 路径白名单双重保证。
// 调用方式: 系统启动时调用一次（非线程安全）。
func RegisterBuiltinTools(
	sbx *sandbox.InProcessSandbox,
	toolReg *tool.InMemoryToolRegistry,
	allowedPaths []string, // 文件系统路径白名单（read_file/list_dir/write_file 均受限）
	dialer protocol.SafeDialer,
	sandboxEnabled bool, // 是否启用平台原生进程沙箱
	netPolicy toolsb.NetworkPolicy, // bash/run_command 网络访问策略
	bwrapPath string, // Linux: bwrap 路径（空=自动查找）
	cfg *config.Config,
	cronRepo protocol.CronRepository, // cron_* 工具依赖；nil 时不注册这三个工具
) error {
	// todoMu 保护 todo 文件的并发读写，防止多 Agent 同时写入导致数据丢失。
	// 与 makeTodoWriteFn / makeTodoReadFn 共享，通过参数传递而非全局变量。
	todoMu := new(sync.Mutex)

	// 元数据与实现绑定表：name → InProcessFn
	// 元数据从 builtin/<name>/tool.yaml + schema.json 加载，不再硬编码在此处。
	defs := []struct {
		name string
		fn   sandbox.InProcessFn
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
		{"glob", makeGlobFn(allowedPaths)},
		{"web_search", makeWebSearchFn(cfg, dialer)},
		{"todo_write", makeTodoWriteFn(allowedPaths, todoMu)},
		{"todo_read", makeTodoReadFn(allowedPaths, todoMu)},
		{"multi_edit", makeMultiEditFn(allowedPaths)},
		{"notebook_read", makeNotebookReadFn(allowedPaths)},
		{"notebook_edit", makeNotebookEditFn(allowedPaths)},
		{"grep", makeGrepFn(allowedPaths)},
		{"git_diff", makeGitDiffFn(allowedPaths)},
		{"git_commit", makeGitCommitFn(allowedPaths)},
		{"template_render", templateRenderFn},
		{"tool_search", tool.MakeToolSearchFn(toolReg)},
		{"data_query", makeDataQueryFn(allowedPaths)},
	}

	// cron_* 工具依赖，仅在 cronRepo != nil 时注册（单元测试无 Repo 时不报错）
	if cronRepo != nil {
		defs = append(defs, []struct {
			name string
			fn   sandbox.InProcessFn
		}{
			{"cron_list", makeCronListFn(cronRepo)},
			{"cron_create", makeCronCreateFn(cronRepo)},
			{"cron_delete", makeCronDeleteFn(cronRepo)},
		}...)
	}

	for _, d := range defs {
		meta, err := tool.LoadBuiltinToolMeta(d.name)
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal,
				fmt.Sprintf("builtin_tools: load meta for %q", d.name), err)
		}
		sbx.Register(meta.Name, d.fn)
		if err := toolReg.Register(meta); err != nil {
			return apperr.Wrap(apperr.CodeInternal,
				fmt.Sprintf("builtin_tools: register %q", d.name), err)
		}
	}

	richDefs := []struct {
		name string
		fn   sandbox.InProcessRichFn
	}{
		{"execute_wasm", makeExecuteWasmFn(allowedPaths)},
	}

	for _, d := range richDefs {
		meta, err := tool.LoadBuiltinToolMeta(d.name)
		if err != nil {
			slog.Warn("builtin_tools: skipped tool (missing metadata)", "tool", d.name, "err", err)
			continue
		}
		sbx.RegisterRich(meta.Name, d.fn, types.TaintHigh)
		if err := toolReg.Register(meta); err != nil {
			return apperr.Wrap(apperr.CodeInternal,
				fmt.Sprintf("builtin_tools: register %q", d.name), err)
		}
	}

	return nil
}

// ── 以下为纯实现函数，不含任何元数据 ─────────────────────────────────────────

// ─── read_file ────────────────────────────────────────────────────────────────

type readFileArgs struct {
	Path string `json:"path"`
}

func makeReadFileFn(allowedPaths []string) sandbox.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		var args readFileArgs
		if err := json.Unmarshal(input, &args); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "read_file: invalid args", err)
		}
		if err := checkAllowedPath(args.Path, allowedPaths); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "makeReadFileFn", err)
		}

		data, err := os.ReadFile(filepath.Clean(args.Path))
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "read_file", err)
		}
		return data, nil
	}
}

// ─── read_tool_ref ────────────────────────────────────────────────────────────

type readToolRefArgs struct {
	ID string `json:"id"`
}

func makeReadToolRefFn() sandbox.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		var args readToolRefArgs
		if err := json.Unmarshal(input, &args); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "read_tool_ref: invalid args", err)
		}
		if args.ID == "" {
			return nil, apperr.New(apperr.CodeInternal, "read_tool_ref: id is required")
		}

		home, err := os.UserHomeDir()
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "read_tool_ref: home dir not found", err)
		}

		// Security: prevent path traversal
		cleanID := filepath.Base(args.ID)
		path := filepath.Join(home, ".polarisagi", "polaris", "data", "tool_refs", cleanID+".log")

		data, err := os.ReadFile(path)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "read_tool_ref: file read error", err)
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

func makeListDirFn(allowedPaths []string) sandbox.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		var args listDirArgs
		if err := json.Unmarshal(input, &args); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "list_dir: invalid args", err)
		}
		if err := checkAllowedPath(args.Path, allowedPaths); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "makeListDirFn", err)
		}

		entries, err := os.ReadDir(filepath.Clean(args.Path))
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "list_dir", err)
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

func makeWriteFileFn(allowedPaths []string) sandbox.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		var args writeFileArgs
		if err := json.Unmarshal(input, &args); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "write_file: invalid args", err)
		}
		if err := checkAllowedPath(args.Path, allowedPaths); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "makeWriteFileFn", err)
		}
		if err := checkForbiddenPath(args.Path); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "makeWriteFileFn", err)
		}

		flag := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
		if args.Append {
			flag = os.O_WRONLY | os.O_CREATE | os.O_APPEND
		}

		f, err := os.OpenFile(filepath.Clean(args.Path), flag, 0o600)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "write_file", err)
		}
		defer f.Close()

		if _, err := f.WriteString(args.Content); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "write_file: write error", err)
		}
		return []byte(`{"written":true}`), nil
	}
}

func checkForbiddenPath(path string) error {
	cleanPath := filepath.Clean(path)
	home, err := os.UserHomeDir()
	var forbidden []string
	if err == nil {
		forbidden = []string{
			filepath.Join(home, ".polarisagi", "polaris", "config"),
			filepath.Join(home, ".polarisagi", "polaris", "data"),
			filepath.Join(home, ".polarisagi", "polaris", "secrets"),
			filepath.Join(home, ".polarisagi", "polaris", "audit"),
			filepath.Join(home, ".ssh"),
			filepath.Join(home, ".gnupg"),
			"/etc",
			"/usr",
			"/bin",
			"/sbin",
		}
	} else {
		forbidden = []string{"/etc", "/usr", "/bin", "/sbin", "/root/.polarisagi"}
	}

	for _, f := range forbidden {
		if cleanPath == f || strings.HasPrefix(cleanPath, f+string(filepath.Separator)) {
			return apperr.New(apperr.CodeForbidden, fmt.Sprintf("write_file: path is in forbidden directory: %s", path))
		}
	}
	return nil
}

// ─── fetch_url ────────────────────────────────────────────────────────────────

type fetchURLArgs struct {
	URL string `json:"url"`
}

// makeFetchURLFn 返回 fetch_url 工具函数。
func makeFetchURLFn(dialer protocol.SafeDialer) sandbox.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		if dialer == nil {
			return nil, apperr.New(apperr.CodeInternal, "fetch_url: SafeDialer is required (XR-06 violation prevented)")
		}

		client := &http.Client{
			Transport: &http.Transport{
				DialContext: dialer.DialContext,
			},
			Timeout: 30 * time.Second,
		}

		var args fetchURLArgs
		if err := json.Unmarshal(input, &args); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "fetch_url: invalid args", err)
		}
		if args.URL == "" {
			return nil, apperr.New(apperr.CodeInternal, "fetch_url: url is required")
		}

		// SSRF Guard Phase 1: 基础文本正则检查 (SafeDialer 内部会有更严格的解析检查)
		if isPrivateURL(args.URL) {
			return nil, apperr.New(apperr.CodeInternal, fmt.Sprintf("fetch_url: SSRF guard blocked private URL: %s", args.URL))
		}

		req, err := http.NewRequestWithContext(ctx, "GET", args.URL, nil)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "fetch_url: bad request", err)
		}

		// 伪装 User-Agent，避免被简单的爬虫拦截
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")

		resp, err := client.Do(req)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "fetch_url: request failed", err)
		}
		defer resp.Body.Close()

		// 限制读取大小（最大 2MB），防止内存溢出
		bodyReader := io.LimitReader(resp.Body, 2*1024*1024)
		body, err := io.ReadAll(bodyReader)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "fetch_url: read response body failed", err)
		}

		// 如果超出了限制
		truncated := len(body) == 2*1024*1024

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

// baseEnv 返回清理后的安全环境变量集。
//
// 设计：继承宿主 PATH（保留用户安装的 python/node/cargo/go 等），
// 追加平台通用工具目录（Homebrew/nix/cargo/pyenv/nvm 等），
// 剔除高危变量（LD_PRELOAD / DYLD_INSERT_LIBRARIES 等注入向量）。
//
// 修复依据：原来 PATH 硬编码为 "/usr/local/bin:/usr/bin:/bin:..." 会导致
// sandbox 内 python3/node/go 等命令找不到（"command not found"）。
func baseEnv() []string {
	// 构建安全 PATH（继承宿主 + 追加常见工具目录）
	sandboxPath := buildSandboxEnvPath()

	// 基础安全变量
	vars := []string{
		"PATH=" + sandboxPath,
		"TMPDIR=/tmp",
		"TEMP=/tmp",
	}

	// HOME：使用真实 home（工具链需要 ~/.cargo 等目录）
	if home, err := os.UserHomeDir(); err == nil {
		vars = append(vars, "HOME="+home)
	} else {
		vars = append(vars, "HOME=/tmp")
	}

	// 安全白名单变量透传
	safePassthrough := []string{
		"LANG", "LC_ALL", "LC_CTYPE",
		"TZ", "USER", "LOGNAME",
		"PYTHONPATH", "PYTHONDONTWRITEBYTECODE", "VIRTUAL_ENV",
		"NODE_PATH", "NODE_ENV",
		"GOPATH", "GOROOT", "GOMODCACHE", "GOCACHE",
		"CARGO_HOME", "RUSTUP_HOME",
		"JAVA_HOME",
	}
	for _, key := range safePassthrough {
		if val := os.Getenv(key); val != "" {
			vars = append(vars, key+"="+val)
		}
	}

	// 高危变量黑名单（显式排除，确保不通过 os.Environ 漏入）
	// 此函数不调用 os.Environ()，仅白名单透传，因此黑名单是 defense-in-depth。
	return vars
}

// buildSandboxEnvPath 构建沙箱进程的 PATH。
// 策略：宿主 PATH + 平台常见工具目录，去重保序。
func buildSandboxEnvPath() string {
	var parts []string
	seen := map[string]bool{}
	add := func(p string) {
		if p != "" && !seen[p] {
			parts = append(parts, p)
			seen[p] = true
		}
	}

	// 1. 继承宿主 PATH（最重要：pyenv/nvm/cargo shims 等需要宿主 PATH）
	for _, p := range filepath.SplitList(os.Getenv("PATH")) {
		add(p)
	}
	// 2. 平台基础目录（保底）
	for _, p := range []string{"/usr/local/bin", "/usr/local/sbin", "/usr/bin", "/usr/sbin", "/bin", "/sbin"} {
		add(p)
	}
	// 3. macOS Homebrew
	if runtime.GOOS == "darwin" {
		add("/opt/homebrew/bin")
		add("/opt/homebrew/sbin")
	}
	// 4–6. nix + 用户工具目录 + Linux 特定（各自提取以降低圈复杂度）
	sandboxPathAddNix(add)
	sandboxPathAddUserTools(add)
	sandboxPathAddLinux(add)

	return strings.Join(parts, string(filepath.ListSeparator))
}

// sandboxPathAddNix 添加 nix/NixOS PATH 目录（存在则加入）。
func sandboxPathAddNix(add func(string)) {
	for _, p := range []string{
		"/nix/var/nix/profiles/default/bin",
		"/run/current-system/sw/bin",
	} {
		if _, err := os.Stat(p); err == nil {
			add(p)
		}
	}
}

// sandboxPathAddUserTools 添加 HOME 相对的用户级工具目录（存在则加入）。
func sandboxPathAddUserTools(add func(string)) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	for _, p := range []string{
		home + "/.cargo/bin",
		home + "/.local/bin",
		home + "/go/bin",
		home + "/.go/bin",
		home + "/.pyenv/shims",
		home + "/.pyenv/bin",
		home + "/.deno/bin",
		home + "/.bun/bin",
		home + "/.asdf/shims",
		home + "/.asdf/bin",
		home + "/.rye/shims",
		home + "/.local/share/mise/shims",
		home + "/.rbenv/shims",
		home + "/.rbenv/bin",
	} {
		if _, err := os.Stat(p); err == nil {
			add(p)
		}
	}
	// NVM：动态解析活跃版本（~/.nvm/alias/default 记录当前版本）。
	// 旧的 ~/.nvm/versions/node/current/bin 是 symlink，NVM 不保证其存在。
	if nvmBin := nvmNodeBinPath(home); nvmBin != "" {
		add(nvmBin)
	}
}

// nvmNodeBinPath 动态解析 NVM 活跃 Node.js 版本的 bin 目录。
// 优先读取 ~/.nvm/alias/default → 构造 ~/.nvm/versions/node/<ver>/bin；
// 读取失败时降级为扫描 ~/.nvm/versions/node/ 取版本号最大的目录。
func nvmNodeBinPath(home string) string {
	nvmVersionsDir := filepath.Join(home, ".nvm", "versions", "node")
	// 1. 读别名文件解析活跃版本
	if p := nvmActiveVersionBinPath(home, nvmVersionsDir); p != "" {
		return p
	}
	// 2. 降级：扫描目录，取字典序最大的 v* 目录（近似最新版本）
	entries, err := os.ReadDir(nvmVersionsDir)
	if err != nil || len(entries) == 0 {
		return ""
	}
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if e.IsDir() && strings.HasPrefix(e.Name(), "v") {
			p := filepath.Join(nvmVersionsDir, e.Name(), "bin")
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}
	return ""
}

// nvmActiveVersionBinPath 从 ~/.nvm/alias/default 别名文件解析活跃版本对应的 bin 目录。
// LTS 别名（lts/*）追踪一层；失败返回空字符串，由调用方走扫描降级路径。
func nvmActiveVersionBinPath(home, nvmVersionsDir string) string {
	aliasPath := filepath.Join(home, ".nvm", "alias", "default")
	data, err := os.ReadFile(aliasPath)
	if err != nil {
		return ""
	}
	version := strings.TrimSpace(string(data))
	// alias 可能指向 lts/* 二级别名，追踪一层
	if strings.HasPrefix(version, "lts/") || (!strings.HasPrefix(version, "v") && !strings.Contains(version, ".")) {
		ltsAlias := filepath.Join(home, ".nvm", "alias", version)
		if ltsData, err2 := os.ReadFile(ltsAlias); err2 == nil {
			version = strings.TrimSpace(string(ltsData))
		}
	}
	if !strings.HasPrefix(version, "v") {
		version = "v" + version
	}
	p := filepath.Join(nvmVersionsDir, version, "bin")
	if _, err := os.Stat(p); err == nil {
		return p
	}
	return ""
}

// sandboxPathAddLinux 添加 Linux 特定 PATH 目录（snap / conda；存在则加入）。
func sandboxPathAddLinux(add func(string)) {
	if runtime.GOOS != "linux" {
		return
	}
	for _, p := range []string{
		"/snap/bin",
		"/opt/conda/bin", "/opt/miniconda3/bin", "/opt/anaconda3/bin",
	} {
		if _, err := os.Stat(p); err == nil {
			add(p)
		}
	}
}

// execInSandbox 通过 Rust FFI（或 Go 降级）在沙箱中执行命令。
// 返回 (output, cmdErr, sandboxMethod, setupErr)。
// setupErr != nil 表示沙箱初始化失败（非命令执行失败）。
func execInSandbox(ctx context.Context, cfg toolsb.NativeSandboxCfg) ([]byte, error, string, error) {
	goCmd, rustResp, wrapErr := toolsb.WrapBashCmd(ctx, cfg)
	if wrapErr != nil {
		return nil, nil, "", wrapErr
	}
	if rustResp != nil {
		var cmdErr error
		if rustResp.ExitCode != 0 {
			cmdErr = apperr.New(apperr.CodeInternal, fmt.Sprintf("exit status %d", rustResp.ExitCode))
		}
		return []byte(rustResp.Output), cmdErr, rustResp.SandboxMethod, nil
	}
	if goCmd != nil {
		out, cmdErr := goCmd.CombinedOutput()
		return out, cmdErr, "go_fallback", nil
	}
	return nil, nil, "none", nil
}

// execWithoutSandbox 在无沙箱模式下执行 bash 命令（仅 env 清理，无进程隔离）。
// 适用于 sandbox.enabled=false 的调试环境；生产路径应走 WrapBashCmd。
// Linux namespace 隔离已移除——统一由 Rust 沙箱（bwrap/Seatbelt）负责进程边界。
func execWithoutSandbox(ctx context.Context, command, workDir string, env []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "bash", "-c", command)
	cmd.Dir = workDir
	cmd.Env = env
	return cmd.CombinedOutput()
}

func makeBashFn(allowedPaths []string, sandboxEnabled bool, netPolicy toolsb.NetworkPolicy, bwrapPath string) sandbox.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		var args bashArgs
		if err := json.Unmarshal(input, &args); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "bash: invalid args", err)
		}
		if args.Command == "" {
			return nil, apperr.New(apperr.CodeInternal, "bash: command is required")
		}

		// ── 安全审核：CommandRiskClassifier ──────────────────────────────────
		// DENY → 直接拒绝，不执行。HITL → 当前 Phase1 记录日志 + 执行（Phase2 挂起等待审批）。
		// WARN → 强化审计日志 + 执行。SAFE → 直接执行。
		verdict := classifier.Default().Classify(args.Command)
		switch verdict.Level {
		case classifier.RiskDeny:
			slog.Error("bash: command DENIED by risk classifier",
				"cmd", args.Command, "reason", verdict.Reason)
			return nil, apperr.New(apperr.CodeForbidden,
				fmt.Sprintf("bash: command denied: %s", verdict.Reason))
		case classifier.RiskHITL:
			// Phase1: 警告日志 + 继续执行（Phase2 将挂起等待 HITL 审批）
			slog.Warn("bash: command requires human approval (HITL) — executing in Phase1 mode",
				"cmd", args.Command, "reason", verdict.Reason)
		case classifier.RiskWarn:
			slog.Warn("bash: elevated-risk command executing",
				"cmd", args.Command, "reason", verdict.Reason)
		}

		workDir := ""
		if len(allowedPaths) > 0 {
			workDir = allowedPaths[0]
		}

		execCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()

		slog.Info("bash: executing command",
			"sandbox_enabled", sandboxEnabled,
			"network", netPolicy,
			"risk", verdict.Level.String(),
			"cmd", args.Command,
			"dir", workDir)

		var outBytes []byte
		var execErr error
		var sandboxMethod string

		if sandboxEnabled {
			cfg := toolsb.NativeSandboxCfg{
				Command:       args.Command,
				WorkDir:       workDir,
				AllowedPaths:  allowedPaths,
				NetworkPolicy: netPolicy,
				Env:           baseEnv(),
				BwrapPath:     bwrapPath,
				TimeoutMs:     30_000,
			}
			var setupErr error
			outBytes, execErr, sandboxMethod, setupErr = execInSandbox(execCtx, cfg)
			if setupErr != nil {
				return nil, apperr.Wrap(apperr.CodeInternal, "bash: sandbox wrap failed", setupErr)
			}
		} else {
			// 沙箱禁用：env 清理 + workDir + Linux namespace（最后防线）
			sandboxMethod = "disabled"
			outBytes, execErr = execWithoutSandbox(execCtx, args.Command, workDir, baseEnv())
		}

		result := map[string]any{
			"command":         args.Command,
			"output":          string(outBytes),
			"exit_code":       0,
			"sandbox_enabled": sandboxEnabled,
			"sandbox_method":  sandboxMethod,
			"network_policy":  string(netPolicy),
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

func getDatetimeFn(_ context.Context, _ []byte) ([]byte, error) {
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

func sysProbeFn(_ context.Context, _ []byte) ([]byte, error) {
	info := sysinfo.GetSystemInfo()
	return json.Marshal(info)
}

// ─── csv_parse ────────────────────────────────────────────────────────────────

type csvParseArgs struct {
	CSV string `json:"csv"`
}

func csvParseFn(_ context.Context, input []byte) ([]byte, error) {
	var args csvParseArgs
	if err := json.Unmarshal(input, &args); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "csv_parse: invalid args", err)
	}
	if args.CSV == "" {
		return nil, apperr.New(apperr.CodeInternal, "csv_parse: csv is required")
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

func diffTextFn(_ context.Context, input []byte) ([]byte, error) {
	var args diffTextArgs
	if err := json.Unmarshal(input, &args); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "diff_text: invalid args", err)
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

// ─── grep ─────────────────────────────────────────────────────────────────────

// grepArgs 是 grep 工具的入参。字段文档见 builtin/grep/schema.json。
type grepArgs struct {
	Pattern         string `json:"pattern"`
	Path            string `json:"path"`
	Glob            string `json:"glob"`
	OutputMode      string `json:"output_mode"`
	ContextBefore   int    `json:"context_before"`
	ContextAfter    int    `json:"context_after"`
	CaseInsensitive bool   `json:"case_insensitive"`
	HeadLimit       int    `json:"head_limit"`
}

type grepMatch struct {
	File          string   `json:"file"`
	Line          int      `json:"line"`
	Text          string   `json:"text"`
	ContextBefore []string `json:"context_before,omitempty"`
	ContextAfter  []string `json:"context_after,omitempty"`
}

type grepFileCount struct {
	File  string `json:"file"`
	Count int    `json:"count"`
}

// grepRunner 封装单次 grep 调用的全部可变状态，将高圈复杂度拆分到多个小方法。
type grepRunner struct {
	re        *regexp.Regexp
	args      grepArgs
	mode      string
	limit     int
	matches   []grepMatch
	files     []string
	counts    []grepFileCount
	total     int
	truncated bool
	seenFiles map[string]struct{}
}

func newGrepRunner(re *regexp.Regexp, args grepArgs) *grepRunner {
	mode := args.OutputMode
	if mode == "" {
		mode = "files_with_matches"
	}
	limit := args.HeadLimit
	if limit <= 0 {
		limit = 250
	}
	if limit > 1000 {
		limit = 1000
	}
	return &grepRunner{
		re:        re,
		args:      args,
		mode:      mode,
		limit:     limit,
		seenFiles: make(map[string]struct{}),
	}
}

// grepClampArgs 收束上下文行数上限，防止超大上下文造成内存压力。
func grepClampArgs(args *grepArgs) {
	if args.ContextBefore < 0 {
		args.ContextBefore = 0
	}
	if args.ContextAfter < 0 {
		args.ContextAfter = 0
	}
	if args.ContextBefore > 10 {
		args.ContextBefore = 10
	}
	if args.ContextAfter > 10 {
		args.ContextAfter = 10
	}
}

// grepValidateMode 校验 output_mode 合法性。
func grepValidateMode(mode string) error {
	switch mode {
	case "", "content", "files_with_matches", "count":
		return nil
	default:
		return apperr.New(apperr.CodeInternal, fmt.Sprintf("grep: unknown output_mode %q", mode))
	}
}

// isBinaryData 检测前 512 字节是否含 null，是则视为二进制文件，跳过以避免乱码输出。
func isBinaryData(data []byte) bool {
	probe := data
	if len(probe) > 512 {
		probe = probe[:512]
	}
	for _, b := range probe {
		if b == 0 {
			return true
		}
	}
	return false
}

// walk 实现 fs.WalkDirFunc，每个文件调用一次。
func (g *grepRunner) walk(path string, d os.DirEntry, walkErr error) error {
	if walkErr != nil {
		return nil //nolint:nilerr // 目录项读取失败时静默跳过，不中断整体 walk
	}
	if d.IsDir() {
		return nil
	}
	if g.truncated {
		return filepath.SkipAll
	}
	if g.args.Glob != "" {
		if matched, _ := doublestar.Match(g.args.Glob, filepath.Base(path)); !matched {
			return nil
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil //nolint:nilerr // 权限不足等情况静默跳过
	}
	if isBinaryData(data) {
		return nil
	}
	return g.scanFile(path, strings.Split(string(data), "\n"))
}

// scanFile 扫描单个文件的所有行，更新 runner 内部状态。
func (g *grepRunner) scanFile(path string, lines []string) error {
	matchCount := 0
	hasMatch := false
	for i, line := range lines {
		if !g.re.MatchString(line) {
			continue
		}
		matchCount++
		hasMatch = true
		if err := g.handleMatch(path, i, line, lines); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "grepRunner.scanFile", err) // filepath.SkipAll 会向上传递至 WalkDir
		}
		if g.mode == "files_with_matches" {
			break // 每文件只记录一次，无需扫描剩余行
		}
	}
	return g.postFile(path, matchCount, hasMatch)
}

// handleMatch 处理单行匹配，按 mode 分发写入结果。
func (g *grepRunner) handleMatch(path string, i int, line string, lines []string) error {
	switch g.mode {
	case "content":
		g.matches = append(g.matches, g.buildMatch(path, i, line, lines))
		if len(g.matches) >= g.limit {
			g.truncated = true
			return filepath.SkipAll
		}
	case "files_with_matches":
		if _, seen := g.seenFiles[path]; !seen {
			g.seenFiles[path] = struct{}{}
			g.files = append(g.files, path)
		}
	}
	return nil
}

// postFile 在文件扫描完成后执行 limit 检查（count / files_with_matches）。
func (g *grepRunner) postFile(path string, matchCount int, hasMatch bool) error {
	if g.mode == "files_with_matches" && len(g.files) >= g.limit {
		g.truncated = true
		return filepath.SkipAll
	}
	if g.mode == "count" && hasMatch {
		g.total += matchCount
		g.counts = append(g.counts, grepFileCount{File: path, Count: matchCount})
		if len(g.counts) >= g.limit {
			g.truncated = true
			return filepath.SkipAll
		}
	}
	return nil
}

// buildMatch 构造含上下文行的匹配记录（仅 content 模式使用）。
func (g *grepRunner) buildMatch(path string, i int, line string, lines []string) grepMatch {
	m := grepMatch{File: path, Line: i + 1, Text: line}
	if g.args.ContextBefore > 0 {
		start := i - g.args.ContextBefore
		if start < 0 {
			start = 0
		}
		m.ContextBefore = lines[start:i]
	}
	if g.args.ContextAfter > 0 {
		end := i + 1 + g.args.ContextAfter
		if end > len(lines) {
			end = len(lines)
		}
		m.ContextAfter = lines[i+1 : end]
	}
	return m
}

// result 序列化最终输出，按 mode 返回对应结构。
func (g *grepRunner) result() ([]byte, error) {
	switch g.mode {
	case "content":
		return json.Marshal(map[string]any{"matches": g.matches, "truncated": g.truncated})
	case "files_with_matches":
		return json.Marshal(map[string]any{"files": g.files, "truncated": g.truncated})
	case "count":
		return json.Marshal(map[string]any{"counts": g.counts, "total": g.total, "truncated": g.truncated})
	default:
		return nil, apperr.New(apperr.CodeInternal, "grep: unreachable")
	}
}

func makeGrepFn(allowedPaths []string) sandbox.InProcessFn {
	return func(_ context.Context, input []byte) ([]byte, error) {
		var args grepArgs
		if err := json.Unmarshal(input, &args); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "grep: invalid args", err)
		}
		if args.Pattern == "" {
			return nil, apperr.New(apperr.CodeInternal, "grep: pattern is required")
		}
		if len(allowedPaths) == 0 {
			return nil, apperr.New(apperr.CodeInternal, "grep: no allowed paths configured")
		}
		if err := grepValidateMode(args.OutputMode); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "makeGrepFn", err)
		}

		reStr := args.Pattern
		if args.CaseInsensitive {
			reStr = "(?i)" + reStr
		}
		re, err := regexp.Compile(reStr)
		if err != nil {
			return nil, apperr.New(apperr.CodeInternal, fmt.Sprintf("grep: invalid pattern: %v", err))
		}

		searchRoots := allowedPaths
		if args.Path != "" {
			if err := checkAllowedPath(args.Path, allowedPaths); err != nil {
				return nil, apperr.Wrap(apperr.CodeInternal, "makeGrepFn", err)
			}
			searchRoots = []string{filepath.Clean(args.Path)}
		}

		grepClampArgs(&args)
		runner := newGrepRunner(re, args)

		for _, root := range searchRoots {
			if walkErr := filepath.WalkDir(filepath.Clean(root), runner.walk); walkErr != nil {
				slog.Warn("grep: walk error", "root", root, "err", walkErr)
			}
			if runner.truncated {
				break
			}
		}

		return runner.result()
	}
}

// ─── 辅助函数 ─────────────────────────────────────────────────────────────────

// checkAllowedPath 确认 path 在白名单内（防路径穿越）。
// 若白名单为空则拒绝所有访问（fail-closed）。
// 使用 == 或 Separator 前缀双重校验，防止 /allowed-extra 通过 /allowed 白名单。
func checkAllowedPath(path string, allowedPaths []string) error {
	if len(allowedPaths) == 0 {
		return apperr.New(apperr.CodeInternal, "path_guard: no allowed paths configured (fail-closed)")
	}
	clean := filepath.Clean(path)
	for _, allowed := range allowedPaths {
		allowedClean := filepath.Clean(allowed)
		if clean == allowedClean || strings.HasPrefix(clean, allowedClean+string(filepath.Separator)) {
			return nil
		}
	}
	return apperr.New(apperr.CodeInternal, fmt.Sprintf("path_guard: path %q not in allowed paths", path))
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

func makeStrReplaceEditorFn(allowedPaths []string) sandbox.InProcessFn {
	// undoBuffer 保存最近一次 str_replace_editor 修改的文件备份（undo_edit 恢复用）。
	// DAGExecutor 并发执行节点时多个 goroutine 可能同时调用 str_replace_editor，必须加锁保护。
	undoBuffer := make(map[string]string)
	var undoBufferMu sync.Mutex
	return func(ctx context.Context, input []byte) ([]byte, error) {
		var args strReplaceEditorArgs
		if err := json.Unmarshal(input, &args); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "str_replace_editor: invalid args", err)
		}
		if err := checkAllowedPath(args.Path, allowedPaths); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "makeStrReplaceEditorFn", err)
		}

		cleanPath := filepath.Clean(args.Path)

		switch args.Command {
		case "create":
			if _, err := os.Stat(cleanPath); err == nil {
				return nil, apperr.New(apperr.CodeInternal, "str_replace_editor: file already exists")
			}
			if err := os.WriteFile(cleanPath, []byte(args.NewStr), 0600); err != nil {
				return nil, apperr.Wrap(apperr.CodeInternal, "str_replace_editor: create failed", err)
			}
			return []byte(`{"status":"created"}`), nil

		case "view":
			data, err := os.ReadFile(cleanPath)
			if err != nil {
				return nil, apperr.Wrap(apperr.CodeInternal, "str_replace_editor: view failed", err)
			}
			return data, nil

		case "str_replace":
			return executeStrReplace(cleanPath, args, undoBuffer, &undoBufferMu)

		case "undo_edit":
			undoBufferMu.Lock()
			oldContent, ok := undoBuffer[cleanPath]
			if ok {
				delete(undoBuffer, cleanPath)
			}
			undoBufferMu.Unlock()
			if !ok {
				return nil, apperr.New(apperr.CodeInternal, "str_replace_editor: no undo history found for this file")
			}
			if err := os.WriteFile(cleanPath, []byte(oldContent), 0600); err != nil {
				return nil, apperr.Wrap(apperr.CodeInternal, "str_replace_editor: undo write failed", err)
			}
			return []byte(`{"status":"undone"}`), nil

		default:
			return nil, apperr.New(apperr.CodeInternal, fmt.Sprintf("str_replace_editor: unknown command %q", args.Command))
		}
	}
}

func executeStrReplace(cleanPath string, args strReplaceEditorArgs, undoBuffer map[string]string, mu *sync.Mutex) ([]byte, error) {
	data, err := os.ReadFile(cleanPath)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "str_replace_editor: read failed", err)
	}
	content := string(data)

	if args.OldStr == "" {
		return nil, apperr.New(apperr.CodeInternal, "str_replace_editor: old_str cannot be empty")
	}

	count := strings.Count(content, args.OldStr)
	if count == 0 {
		return nil, apperr.New(apperr.CodeInternal, "str_replace_editor: old_str not found in file")
	}
	if count > 1 {
		return nil, apperr.New(apperr.CodeInternal, "str_replace_editor: old_str is not unique, matched multiple times. Please provide more context in old_str.")
	}

	// 备份到 undoBuffer（加锁：多个节点并发执行 str_replace_editor 时防竞争）
	mu.Lock()
	undoBuffer[cleanPath] = content
	mu.Unlock()

	newContent := strings.Replace(content, args.OldStr, args.NewStr, 1)
	if err := os.WriteFile(cleanPath, []byte(newContent), 0600); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "str_replace_editor: write failed", err)
	}
	return []byte(`{"status":"replaced"}`), nil
}

// ─── run_command ─────────────────────────────────────────────────────────────

type runCommandArgs struct {
	Command    string `json:"command"`
	WorkingDir string `json:"working_dir"`
	TimeoutS   int    `json:"timeout_s"`
}

func parseRunCommandArgs(input []byte, allowedPaths []string) (*runCommandArgs, string, time.Duration, error) {
	var args runCommandArgs
	if err := json.Unmarshal(input, &args); err != nil {
		return nil, "", 0, apperr.Wrap(apperr.CodeInternal, "run_command: invalid args", err)
	}
	if args.Command == "" {
		return nil, "", 0, apperr.New(apperr.CodeInternal, "run_command: command is required")
	}

	// 命令前缀白名单（构建工具，不含 bash/sh 等 shell 解释器）
	cmdPrefix := strings.SplitN(strings.TrimSpace(args.Command), " ", 2)[0]
	allowedCmds := map[string]bool{
		"go": true, "cargo": true, "npm": true, "yarn": true, "pnpm": true,
		"make": true, "pytest": true, "tsc": true, "python": true, "python3": true,
		"pip": true, "pip3": true, "node": true, "deno": true, "bun": true,
	}
	if !allowedCmds[cmdPrefix] {
		return nil, "", 0, apperr.New(apperr.CodeForbidden, fmt.Sprintf("run_command: command %q not in whitelist", cmdPrefix))
	}

	workDir := args.WorkingDir
	if workDir == "" && len(allowedPaths) > 0 {
		workDir = allowedPaths[0]
	}
	if workDir != "" {
		if err := checkAllowedPath(workDir, allowedPaths); err != nil {
			return nil, "", 0, apperr.Wrap(apperr.CodeInternal, "makeRunCommandFn", err)
		}
	}

	timeout := time.Duration(args.TimeoutS) * time.Second
	if timeout <= 0 || timeout > 120*time.Second {
		timeout = 30 * time.Second
	}

	return &args, workDir, timeout, nil
}

func makeRunCommandFn(allowedPaths []string, sandboxEnabled bool, netPolicy toolsb.NetworkPolicy, bwrapPath string) sandbox.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		args, workDir, timeout, err := parseRunCommandArgs(input, allowedPaths)
		if err != nil {
			return nil, err
		}

		execCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()

		// ── 安全审核：CommandRiskClassifier ──────────────────────────────────
		verdict := classifier.Default().Classify(args.Command)
		switch verdict.Level {
		case classifier.RiskDeny:
			slog.Error("run_command: command DENIED by risk classifier",
				"cmd", args.Command, "reason", verdict.Reason)
			return nil, apperr.New(apperr.CodeForbidden,
				fmt.Sprintf("run_command: command denied: %s", verdict.Reason))
		case classifier.RiskHITL:
			slog.Warn("run_command: command requires human approval (HITL) — executing in Phase1 mode",
				"cmd", args.Command, "reason", verdict.Reason)
		case classifier.RiskWarn:
			slog.Warn("run_command: elevated-risk command executing",
				"cmd", args.Command, "reason", verdict.Reason)
		}

		slog.Info("run_command: executing",
			"sandbox_enabled", sandboxEnabled,
			"network", netPolicy,
			"risk", verdict.Level.String(),
			"cmd", args.Command,
			"dir", workDir)

		env := append(baseEnv(), "GOCACHE=/tmp/gocache", "CARGO_HOME=/tmp/cargo", "npm_config_cache=/tmp/npm")

		var outBytes []byte
		var execErr error
		var sandboxMethod string

		if sandboxEnabled {
			cfg := toolsb.NativeSandboxCfg{
				Command:       args.Command,
				WorkDir:       workDir,
				AllowedPaths:  allowedPaths,
				NetworkPolicy: netPolicy, // 构建工具通常需要网络（下载依赖），由上层配置控制
				Env:           env,
				BwrapPath:     bwrapPath,
				TimeoutMs:     uint64(timeout.Milliseconds()),
			}
			var setupErr error
			outBytes, execErr, sandboxMethod, setupErr = execInSandbox(execCtx, cfg)
			if setupErr != nil {
				return nil, apperr.Wrap(apperr.CodeInternal, "run_command: sandbox wrap failed", setupErr)
			}
		} else {
			sandboxMethod = "disabled"
			outBytes, execErr = execWithoutSandbox(execCtx, args.Command, workDir, env)
		}

		result := map[string]any{
			"command":        args.Command,
			"output":         string(outBytes),
			"exit_code":      0,
			"sandbox_method": sandboxMethod,
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

// ─── glob ────────────────────────────────────────────────────────────────────

func makeGlobFn(allowedPaths []string) sandbox.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		var args struct {
			Pattern string `json:"pattern"`
		}
		if err := json.Unmarshal(input, &args); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "glob: invalid args", err)
		}
		if len(allowedPaths) == 0 {
			return nil, apperr.New(apperr.CodeInternal, "glob: no allowed paths configured")
		}

		// 遍历所有允许路径，而非仅第一个
		var fullPaths []string
		for _, workDir := range allowedPaths {
			fsys := os.DirFS(workDir)
			// os.DirFS 限定了根目录，doublestar.Glob 不会跨越边界
			matches, err := doublestar.Glob(fsys, args.Pattern)
			if err != nil {
				return nil, apperr.Wrap(apperr.CodeInternal, "glob: error matching", err)
			}
			for _, m := range matches {
				fullPaths = append(fullPaths, filepath.Join(workDir, m))
			}
		}
		return json.Marshal(map[string]any{"matches": fullPaths})
	}
}

// ─── web_search ──────────────────────────────────────────────────────────────

func makeWebSearchFn(cfg *config.Config, dialer protocol.SafeDialer) sandbox.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		var args struct {
			Query string `json:"query"`
		}
		if err := json.Unmarshal(input, &args); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "web_search: invalid args", err)
		}
		if dialer == nil {
			return nil, apperr.New(apperr.CodeInternal, "web_search: SafeDialer is required")
		}
		// Query 长度防护：防止超大查询消耗带宽和下游资源
		if len(args.Query) == 0 {
			return nil, apperr.New(apperr.CodeInternal, "web_search: query is empty")
		}
		if len(args.Query) > 500 {
			return nil, apperr.New(apperr.CodeInternal, "web_search: query exceeds 500 chars")
		}

		client := &http.Client{
			Transport: &http.Transport{DialContext: dialer.DialContext},
			Timeout:   30 * time.Second,
		}

		// MVP: DuckDuckGo HTML scraping
		req, err := http.NewRequestWithContext(ctx, "GET", "https://html.duckduckgo.com/html/?q="+url.QueryEscape(args.Query), nil)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "makeWebSearchFn", err)
		}
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64)")
		resp, err := client.Do(req)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "web_search: req failed", err)
		}
		defer resp.Body.Close()
		// 限制读取大小（2MB），防止超大响应体导致内存耗尽
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))

		tagRe := regexp.MustCompile(`<[^>]*>`)
		spaceRe := regexp.MustCompile(`\s+`)
		snippets := regexp.MustCompile(`(?s)<a class="result__snippet[^>]*>(.*?)</a>`).FindAllStringSubmatch(string(body), 10)

		var results []string
		for _, s := range snippets {
			txt := tagRe.ReplaceAllString(s[1], " ")
			txt = strings.TrimSpace(spaceRe.ReplaceAllString(txt, " "))
			results = append(results, txt)
		}
		return json.Marshal(map[string]any{"results": results})
	}
}

// ─── todo_write & todo_read ──────────────────────────────────────────────────

func getTodoPath(allowedPaths []string) (string, error) {
	if len(allowedPaths) == 0 {
		return "", apperr.New(apperr.CodeInternal, "todo: no workspace configured")
	}
	return filepath.Join(allowedPaths[0], ".polaris_todo.json"), nil
}

// makeTodoWriteFn 创建 todo_write 工具函数。
// mu 由调用方（RegisterBuiltinTools）创建并同时传给 makeTodoReadFn，保证读写共享同一把锁。
func makeTodoWriteFn(allowedPaths []string, mu *sync.Mutex) sandbox.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		var args struct {
			Todos []string `json:"todos"`
		}
		if err := json.Unmarshal(input, &args); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "todo_write: invalid args", err)
		}
		path, err := getTodoPath(allowedPaths)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "makeTodoWriteFn", err)
		}
		mu.Lock()
		defer mu.Unlock()
		data, _ := json.MarshalIndent(args.Todos, "", "  ")
		if err := os.WriteFile(path, data, 0600); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "todo_write: write failed", err)
		}
		return []byte(`{"status":"success"}`), nil
	}
}

// makeTodoReadFn 创建 todo_read 工具函数。
// mu 与 makeTodoWriteFn 共享，防止并发读写数据丢失。
func makeTodoReadFn(allowedPaths []string, mu *sync.Mutex) sandbox.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		path, err := getTodoPath(allowedPaths)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "makeTodoReadFn", err)
		}
		mu.Lock()
		defer mu.Unlock()
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return []byte(`{"todos":[]}`), nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "todo_read: read failed", err)
		}
		var todos []string
		if err := json.Unmarshal(data, &todos); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "todo_read: parse failed", err)
		}
		return json.Marshal(map[string]any{"todos": todos})
	}
}

// ─── multi_edit ──────────────────────────────────────────────────────────────

func makeMultiEditFn(allowedPaths []string) sandbox.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		var args struct {
			Path  string `json:"path"`
			Edits []struct {
				OldStr string `json:"old_str"`
				NewStr string `json:"new_str"`
			} `json:"edits"`
		}
		if err := json.Unmarshal(input, &args); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "multi_edit: invalid args", err)
		}
		if err := checkAllowedPath(args.Path, allowedPaths); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "makeMultiEditFn", err)
		}
		cleanPath := filepath.Clean(args.Path)
		data, err := os.ReadFile(cleanPath)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "multi_edit: read failed", err)
		}
		original := string(data)

		// 第一遍：在原始内容中定位所有替换区间，防止链式污染。
		// 链式污染：顺序替换时 edit[0].NewStr 若包含 edit[1].OldStr，
		// 会被 edit[1] 二次替换，产生非预期结果。
		type region struct {
			start  int
			end    int
			newStr string
		}
		regions := make([]region, 0, len(args.Edits))
		for _, edit := range args.Edits {
			if strings.Count(original, edit.OldStr) != 1 {
				return nil, apperr.New(apperr.CodeInternal, fmt.Sprintf("multi_edit: old_str not unique or not found: %q", edit.OldStr))
			}
			idx := strings.Index(original, edit.OldStr)
			regions = append(regions, region{idx, idx + len(edit.OldStr), edit.NewStr})
		}

		// 按起始位置升序排列，便于重叠检测和顺序重建
		sort.Slice(regions, func(i, j int) bool { return regions[i].start < regions[j].start })

		// 检查区间重叠（两个 OldStr 在文件中位置交叉）
		for i := 1; i < len(regions); i++ {
			if regions[i].start < regions[i-1].end {
				return nil, apperr.New(apperr.CodeInternal, "multi_edit: edits overlap in file")
			}
		}

		// 从原始内容重建，避免任何链式副作用
		var buf strings.Builder
		cursor := 0
		for _, r := range regions {
			buf.WriteString(original[cursor:r.start])
			buf.WriteString(r.newStr)
			cursor = r.end
		}
		buf.WriteString(original[cursor:])

		if err := os.WriteFile(cleanPath, []byte(buf.String()), 0600); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "multi_edit: write failed", err)
		}
		return []byte(`{"status":"success"}`), nil
	}
}

// ─── notebook ────────────────────────────────────────────────────────────────

func makeNotebookReadFn(allowedPaths []string) sandbox.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		var args struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(input, &args); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "notebook_read: invalid args", err)
		}
		if err := checkAllowedPath(args.Path, allowedPaths); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "makeNotebookReadFn", err)
		}
		data, err := os.ReadFile(filepath.Clean(args.Path))
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "notebook_read: read failed", err)
		}
		var nb map[string]any
		if err := json.Unmarshal(data, &nb); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "notebook_read: parse failed", err)
		}
		cells, _ := nb["cells"].([]any)
		var out []map[string]any
		for i, c := range cells {
			cell, _ := c.(map[string]any)
			out = append(out, map[string]any{
				"index":     i,
				"cell_type": cell["cell_type"],
				"source":    cell["source"],
				"outputs":   cell["outputs"],
			})
		}
		return json.Marshal(map[string]any{"cells": out})
	}
}

func makeNotebookEditFn(allowedPaths []string) sandbox.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		var args struct {
			Path      string `json:"path"`
			CellIndex int    `json:"cell_index"`
			Source    string `json:"source"`
		}
		if err := json.Unmarshal(input, &args); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "notebook_edit: invalid args", err)
		}
		if err := checkAllowedPath(args.Path, allowedPaths); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "makeNotebookEditFn", err)
		}
		cleanPath := filepath.Clean(args.Path)
		data, err := os.ReadFile(cleanPath)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "notebook_edit: read failed", err)
		}
		var nb map[string]any
		if err := json.Unmarshal(data, &nb); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "notebook_edit: parse failed", err)
		}
		cells, ok := nb["cells"].([]any)
		if !ok || args.CellIndex < 0 || args.CellIndex >= len(cells) {
			return nil, apperr.New(apperr.CodeInternal, "notebook_edit: cell index out of bounds")
		}
		cell, _ := cells[args.CellIndex].(map[string]any)

		// Jupyter source is usually array of strings or a single string
		// Convert new source to array of strings (lines)
		lines := strings.Split(args.Source, "\n")
		var sourceLines []string
		for i, l := range lines {
			if i < len(lines)-1 {
				sourceLines = append(sourceLines, l+"\n")
			} else {
				sourceLines = append(sourceLines, l)
			}
		}
		cell["source"] = sourceLines
		cells[args.CellIndex] = cell
		nb["cells"] = cells

		newData, _ := json.MarshalIndent(nb, "", "  ")
		if err := os.WriteFile(cleanPath, newData, 0600); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "notebook_edit: write failed", err)
		}
		return []byte(`{"status":"success"}`), nil
	}
}

// ─── execute_wasm ─────────────────────────────────────────────────────────────

// isPathAllowed 检查 path 是否在白名单内。
// 安全策略：
//   - fail-closed：白名单为空时拒绝所有路径（防止未配置时全量放行）
//   - 分隔符校验：/workspace-evil 不匹配 /workspace（路径穿越防护）
func isPathAllowed(path string, allowedPaths []string) bool {
	if len(allowedPaths) == 0 {
		return false // fail-closed：空白名单拒绝所有
	}
	cleanPath := filepath.Clean(path)
	for _, allowed := range allowedPaths {
		cleanAllowed := filepath.Clean(allowed)
		// 精确匹配或严格子路径匹配（必须紧跟分隔符，防止前缀混淆）
		if cleanPath == cleanAllowed ||
			strings.HasPrefix(cleanPath, cleanAllowed+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func makeExecuteWasmFn(allowedPaths []string) sandbox.InProcessRichFn {
	return func(ctx context.Context, spec sandbox.SandboxSpec) (*types.ToolResult, error) {
		var args struct {
			Code      string `json:"code"`
			Input     string `json:"input"`
			Network   bool   `json:"network_allowed"`
			MaxPages  int    `json:"max_pages"`
			Workspace string `json:"workspace"`
		}
		if err := json.Unmarshal(spec.Input, &args); err != nil {
			return nil, apperr.Wrap(apperr.CodeInvalidInput, "invalid json", err)
		}

		cleanWorkspace := filepath.Clean(args.Workspace)
		if !isPathAllowed(cleanWorkspace, allowedPaths) {
			return nil, apperr.New(apperr.CodeInternal, "workspace path not allowed")
		}

		quota := toolsb.CalculateWasmQuota(spec.SystemTier, spec.TaintLevel)
		if args.MaxPages > 0 && args.MaxPages < quota.MemoryPages {
			quota.MemoryPages = args.MaxPages
		}

		// 这里实际依赖 toolsb.WasmtimeExecute FFI，如果是在纯 Go 层我们假设其内部处理了隔离
		outJSON, err := toolsb.WasmtimeExecute(
			[]byte(args.Code),
			args.Input,
			cleanWorkspace,
			quota.MemoryPages,
			args.Network,
			quota.Fuel,
			10*1024*1024,
		)

		if err != nil {
			//nolint:nilerr
			return &types.ToolResult{
				Success: false,
				Error:   err.Error(),
			}, nil
		}

		return &types.ToolResult{
			Success: true,
			Output:  []byte(outJSON),
		}, nil
	}
}

// stripSQLComments 移除 SQL 注释（块注释 /* */ 和行注释 --），用于 SELECT-only 校验预处理。
// 防止攻击者通过 `/* DROP TABLE */ SELECT 1` 或 `--\nDROP TABLE` 绕过首 token 检查。
func stripSQLComments(s string) string {
	// 1. 移除块注释 /* ... */（不支持嵌套，符合 SQL 标准）
	var buf strings.Builder
	buf.Grow(len(s))
	i := 0
	for i < len(s) {
		if i+1 < len(s) && s[i] == '/' && s[i+1] == '*' {
			j := i + 2
			for j+1 < len(s) && (s[j] != '*' || s[j+1] != '/') {
				j++
			}
			if j+1 < len(s) {
				j += 2 // 跳过 */
			} else {
				j = len(s) // 未关闭的块注释到末尾
			}
			buf.WriteByte(' ') // 占位保持 token 边界
			i = j
			continue
		}
		buf.WriteByte(s[i])
		i++
	}
	// 2. 移除行注释 -- ...（直到换行符）
	lines := strings.Split(buf.String(), "\n")
	for idx, line := range lines {
		if ci := strings.Index(line, "--"); ci >= 0 {
			lines[idx] = line[:ci]
		}
	}
	return strings.Join(lines, "\n")
}

// makeDataQueryFn 返回 data_query 工具的执行函数。
// 约束：SELECT-only、allowedPaths 路径白名单、参数化查询、只读连接、行数上限。
// 驱动：modernc.org/sqlite（纯 Go，无 CGO，ADR-0003）。
//
//nolint:gocyclo
func makeDataQueryFn(allowedPaths []string) sandbox.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		var args struct {
			Query    string `json:"query"`
			Database string `json:"database"`
			Params   []any  `json:"params"`
			MaxRows  int    `json:"max_rows"`
		}
		if err := json.Unmarshal(input, &args); err != nil {
			return nil, apperr.New(apperr.CodeInternal, "data_query: invalid input JSON")
		}

		// 行数上限校验
		if args.MaxRows <= 0 {
			args.MaxRows = 1000
		}
		if args.MaxRows > 10000 {
			args.MaxRows = 10000
		}

		// 路径白名单校验（与 read_file 共用机制）
		if err := checkAllowedPath(args.Database, allowedPaths); err != nil {
			return nil, apperr.Wrap(apperr.CodeForbidden, "data_query: database path not allowed", err)
		}

		// SELECT-only 校验（R1.15）：
		// 1. 先剥离 SQL 注释（防 /* DROP TABLE */ SELECT ... 绕过）
		// 2. 首 token 必须为 SELECT 或 WITH（CTE 语法）；WITH 必须包含 SELECT
		// 3. 禁止多语句（分号 = 潜在第二条 DDL/DML）
		normalized := stripSQLComments(args.Query)
		trimmed := strings.ToUpper(strings.TrimSpace(normalized))
		if trimmed == "" {
			return nil, apperr.New(apperr.CodeForbidden, "data_query: empty query after stripping comments")
		}
		fields := strings.Fields(trimmed)
		firstKw := fields[0]
		if firstKw != "SELECT" && firstKw != "WITH" {
			preview := trimmed
			if len(preview) > 20 {
				preview = preview[:20] + "..."
			}
			return nil, apperr.New(apperr.CodeForbidden,
				"data_query: only SELECT/WITH...SELECT queries are permitted (got: "+preview+")")
		}
		if firstKw == "WITH" && !strings.Contains(trimmed, "SELECT") {
			return nil, apperr.New(apperr.CodeForbidden, "data_query: WITH clause must contain SELECT")
		}
		if strings.ContainsRune(trimmed, ';') {
			return nil, apperr.New(apperr.CodeForbidden, "data_query: multi-statement queries not permitted")
		}

		// 只读连接（mode=ro 阻止任何写操作在 OS 层）
		dbURI := fmt.Sprintf("file:%s?mode=ro&_busy_timeout=5000", filepath.Clean(args.Database))
		db, err := sql.Open("sqlite", dbURI)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "data_query: open db failed", err)
		}
		defer db.Close()

		// 单连接只读，无需连接池
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)

		// 30 秒查询超时
		qCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()

		rows, err := db.QueryContext(qCtx, args.Query, args.Params...)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "data_query: query failed", err)
		}
		defer rows.Close()

		cols, err := rows.Columns()
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "data_query: get columns failed", err)
		}

		// 收集结果行
		result := make([]map[string]any, 0, min(args.MaxRows, 64))
		truncated := false
		rowCount := 0
		vals := make([]any, len(cols))
		valPtrs := make([]any, len(cols))
		for i := range vals {
			valPtrs[i] = &vals[i]
		}

		for rows.Next() {
			if rowCount >= args.MaxRows {
				truncated = true
				break
			}
			if err := rows.Scan(valPtrs...); err != nil {
				return nil, apperr.Wrap(apperr.CodeInternal, "data_query: scan row failed", err)
			}
			row := make(map[string]any, len(cols))
			for i, col := range cols {
				// SQLite 返回 []byte 时转 string，便于 JSON 序列化
				switch v := vals[i].(type) {
				case []byte:
					row[col] = string(v)
				default:
					row[col] = v
				}
			}
			result = append(result, row)
			rowCount++
		}
		if err := rows.Err(); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "data_query: rows iteration failed", err)
		}

		out, err := json.Marshal(map[string]any{
			"rows":      result,
			"count":     rowCount,
			"truncated": truncated,
			"columns":   cols,
		})
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "data_query: marshal result failed", err)
		}
		return out, nil
	}
}
