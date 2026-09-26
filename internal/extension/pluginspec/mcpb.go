package pluginspec

import (
	"archive/zip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/polarisagi/polaris/pkg/apperr"
)

const (
	RuleMCPBundle       = "mcpb.bundle"
	RuleMCPBundleRemote = "mcpb.remote"
	mcpbCacheDir        = ".mcpb-cache"
	// 解压上限：防 zip 炸弹。MCPB 包内含依赖（node_modules / Python site-packages），
	// 上限取 512MB / 50k 条目，覆盖常见包体的同时让恶意包在磁盘耗尽前失败。
	mcpbMaxUncompressedBytes = 512 << 20
	mcpbMaxEntries           = 50000
)

var errBundleUnsafe = apperr.New(apperr.CodeInvalidInput, "mcpb: unsafe archive")

// mcpbManifest MCP Bundle（.mcpb，旧名 .dxt）manifest.json 中与运行相关的字段。
type mcpbManifest struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Server  struct {
		Type       string `json:"type"`
		EntryPoint string `json:"entry_point"`
		MCPConfig  struct {
			Command           string                     `json:"command"`
			Args              []string                   `json:"args"`
			Env               map[string]string          `json:"env"`
			PlatformOverrides map[string]json.RawMessage `json:"platform_overrides"`
		} `json:"mcp_config"`
	} `json:"server"`
	UserConfig json.RawMessage `json:"user_config"`
}

// mcpbResult 一个包贡献的服务器与包内 user_config 选项（与插件 userConfig 共用 ${user_config.*} 命名空间）。
type mcpbResult struct {
	server     MCPServer
	userConfig []UserConfigOption
}

// loadMCPBundle 解压到 <plugin>/.mcpb-cache/<包名>/（Claude 同一位置）并读取服务器配置。
func loadMCPBundle(root, bundlePath string, ds *diagnostics) (mcpbResult, bool) {
	base := strings.TrimSuffix(filepath.Base(bundlePath), filepath.Ext(bundlePath))
	dest := filepath.Join(root, mcpbCacheDir, base)
	if err := extractBundle(bundlePath, dest); err != nil {
		ds.errorf("mcp", bundlePath, RuleMCPBundle, "extract failed: %v", err)
		return mcpbResult{}, false
	}
	raw, err := os.ReadFile(filepath.Join(dest, "manifest.json"))
	if err != nil {
		ds.errorf("mcp", bundlePath, RuleMCPBundle, "manifest.json missing: %v", err)
		return mcpbResult{}, false
	}
	var m mcpbManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		ds.errorf("mcp", bundlePath, RuleMCPBundle, "invalid manifest.json: %v", err)
		return mcpbResult{}, false
	}
	cfg := m.Server.MCPConfig
	applyPlatformOverride(&cfg.Command, &cfg.Args, &cfg.Env, cfg.PlatformOverrides[mcpbPlatform()])
	name := firstNonEmpty(m.Name, base)
	srv := MCPServer{Name: name, Type: MCPTypeStdio, Command: expandDirname(cfg.Command, dest),
		Env: cfg.Env, Cwd: dest, Source: bundlePath}
	for _, a := range cfg.Args {
		srv.Args = append(srv.Args, expandDirname(a, dest))
	}
	if !validateMCPServer(srv, bundlePath, ds) {
		return mcpbResult{}, false
	}
	var res mcpbResult
	res.server = srv
	if len(m.UserConfig) > 0 {
		res.userConfig = parseUserConfigOptions(m.UserConfig, bundlePath, ds)
	}
	return res, true
}

func applyPlatformOverride(cmd *string, args *[]string, env *map[string]string, raw json.RawMessage) {
	if len(raw) == 0 {
		return
	}
	var o struct {
		Command string            `json:"command"`
		Args    []string          `json:"args"`
		Env     map[string]string `json:"env"`
	}
	if json.Unmarshal(raw, &o) != nil {
		return
	}
	if o.Command != "" {
		*cmd = o.Command
	}
	if o.Args != nil {
		*args = o.Args
	}
	if o.Env != nil {
		*env = o.Env
	}
}

// mcpbPlatform MCPB 使用 Node 风格平台名（win32 / darwin / linux）。
func mcpbPlatform() string {
	if runtime.GOOS == "windows" {
		return "win32"
	}
	return runtime.GOOS
}

func expandDirname(s, dir string) string {
	return strings.ReplaceAll(s, "${__dirname}", filepath.ToSlash(dir))
}

// extractBundle 安全解压：拒绝绝对路径 / ".." / 符号链接条目，限制条目数与解压总量。
func extractBundle(zipPath, dest string) error {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return apperr.Wrap(apperr.CodeInvalidInput, "extractBundle: open", err)
	}
	defer r.Close()
	if len(r.File) > mcpbMaxEntries {
		return apperr.Wrap(apperr.CodeResourceExhausted, "extractBundle: too many entries", errBundleUnsafe)
	}
	if err := os.RemoveAll(dest); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "extractBundle: reset cache", err)
	}
	var total int64
	for _, f := range r.File {
		n, err := extractEntry(f, dest, mcpbMaxUncompressedBytes-total)
		if err != nil {
			return err
		}
		total += n
	}
	return nil
}

func extractEntry(f *zip.File, dest string, budget int64) (int64, error) {
	name := filepath.FromSlash(f.Name)
	target := filepath.Join(dest, name)
	if filepath.IsAbs(name) || !withinRoot(dest, target) || f.Mode()&os.ModeSymlink != 0 {
		return 0, apperr.Wrap(apperr.CodeInvalidInput, "extractBundle: entry "+f.Name, errBundleUnsafe)
	}
	if f.FileInfo().IsDir() {
		if err := os.MkdirAll(target, 0o755); err != nil {
			return 0, apperr.Wrap(apperr.CodeInternal, "extractBundle: mkdir", err)
		}
		return 0, nil
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return 0, apperr.Wrap(apperr.CodeInternal, "extractBundle: mkdir", err)
	}
	src, err := f.Open()
	if err != nil {
		return 0, apperr.Wrap(apperr.CodeInvalidInput, "extractBundle: open entry", err)
	}
	defer src.Close()
	// 只保留可执行位：包内 binary 类型服务器需要执行权限，其余权限位不信任压缩包。
	mode := os.FileMode(0o644)
	if f.Mode()&0o111 != 0 {
		mode = 0o755
	}
	out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return 0, apperr.Wrap(apperr.CodeInternal, "extractBundle: create", err)
	}
	defer out.Close()
	n, err := io.Copy(out, io.LimitReader(src, budget+1))
	if err != nil {
		return n, apperr.Wrap(apperr.CodeInternal, "extractBundle: write", err)
	}
	if n > budget {
		return n, apperr.Wrap(apperr.CodeResourceExhausted, "extractBundle: uncompressed size limit", errBundleUnsafe)
	}
	return n, nil
}
