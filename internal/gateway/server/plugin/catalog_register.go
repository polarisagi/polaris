package plugin

import (
	"github.com/polarisagi/polaris/internal/gateway/types"

	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/polarisagi/polaris/internal/extension/mcp"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/concurrent"
	apptypes "github.com/polarisagi/polaris/pkg/types"
)

// registerPluginMCPServers 将插件 bundle 中所有 MCP 服务器写入 mcp_servers 全局表，
// 并异步启动连接（agentskills.io 标准：plugin 安装时 MCP 自动注册到统一表）。
func (h *PluginHandler) registerPluginMCPServers(ctx context.Context, pluginID, pluginName, installPath string, servers map[string]pluginMCPDef, trustTier int, now string) {
	for srvName, def := range servers {
		transport := "stdio"
		if def.URL != "" {
			transport = "streamable_http"
		}
		argsBytes, _ := json.Marshal(def.Args)
		envMap := def.Env
		if envMap == nil {
			envMap = map[string]string{}
		}
		envBytes, _ := json.Marshal(envMap)
		serverID := fmt.Sprintf("plugin_%s_%s", pluginID, srvName)
		// scopedName：srvName 已是 pluginName 后缀时直接用 pluginName（避免双后缀如
		// "polaris-social-poster-social-poster"）；否则拼接区分同插件多服务器场景。
		scopedName := pluginName
		if pluginName != srvName && !strings.HasSuffix(pluginName, "-"+srvName) {
			scopedName = pluginName + "-" + srvName
		}

		err := h.ExtRepo.UpsertMCPServer(ctx, apptypes.MCPServerRow{
			ID:        serverID,
			Name:      scopedName,
			Transport: transport,
			Command:   def.Command,
			Args:      string(argsBytes),
			Env:       string(envBytes),
			URL:       def.URL,
			Enabled:   true,
			Timeout:   30,
			TrustTier: trustTier,
			CatalogID: "",
			PluginID:  pluginID,
			WorkDir:   installPath,
			CreatedAt: now,
			UpdatedAt: now,
		})
		if err != nil {
			slog.Warn("plugin_catalog: register plugin mcp failed", "server", srvName, "err", err)
			continue
		}

		if h.MCPMgr != nil {
			cfg := types.MCPServerConfig{
				ID: serverID, Name: scopedName, Transport: transport,
				Command: def.Command, Args: def.Args, Env: def.Env,
				URL: def.URL, Timeout: 30, WorkDir: installPath,
				TrustTier: trustTier,
			}
			concurrent.SafeGo(protocol.Detach(ctx), "gateway.plugin.start_mcp_server_register", func(ctx context.Context) {
				_ = h.StartMCPServer(ctx, cfg)
			})
		}
	}
}

// registerPluginSkills 扫描插件 bundle 中声明的 skills 目录，
// 将每个 SKILL.md 注册进 skills 表（agentskills.io 标准：plugin 安装时 skills 自动发现）。
// skillReg 为 nil 时静默跳过（Tier-0 降级）。
func (h *PluginHandler) registerPluginSkills(ctx context.Context, pluginID, pluginName, destDir string, bundle *protocol.PluginBundleManifest, trustTier int) {
	if h.SkillReg == nil {
		return
	}

	// 确定 skills 根目录：manifest 声明 > 约定路径 "skills/"
	skillsRoot := ""
	if bundle.SkillsDir != "" {
		if p, ok := safeJoin(destDir, bundle.SkillsDir); ok {
			skillsRoot = p
		}
	}
	if skillsRoot == "" {
		candidate := filepath.Join(destDir, "skills")
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			skillsRoot = candidate
		}
	}

	// array form（bundle.Skills）：每个 BundleSkillRef.Path 直接指向 SKILL.md
	if len(bundle.Skills) > 0 {
		for _, ref := range bundle.Skills {
			if ref.Path == "" {
				continue
			}
			skillMDPath, ok := safeJoin(destDir, ref.Path)
			if !ok {
				continue
			}
			h.registerOneSkill(ctx, pluginID, pluginName, skillMDPath, trustTier)
		}
		return
	}

	// string form：遍历 skillsRoot 下所有 SKILL.md
	if skillsRoot == "" {
		return
	}
	_ = filepath.WalkDir(skillsRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != "SKILL.md" {
			return nil //nolint:nilerr
		}
		h.registerOneSkill(ctx, pluginID, pluginName, path, trustTier)
		return nil
	})
}

// registerOneSkill 读取单个 SKILL.md 并写入 skills 表。
func (h *PluginHandler) registerOneSkill(ctx context.Context, pluginID, pluginName, skillMDPath string, trustTier int) {
	data, err := os.ReadFile(skillMDPath)
	if err != nil {
		slog.Warn("plugin_catalog: cannot read SKILL.md", "path", skillMDPath, "err", err)
		return
	}
	fm := parseFrontmatter(string(data))

	// skill 名称：优先 frontmatter name；否则取目录名
	skillSlug := fm.Name
	if skillSlug == "" {
		skillSlug = filepath.Base(filepath.Dir(skillMDPath))
	}
	// skillSlug 来自文件系统/frontmatter，可能含空格、点、斜杠等非法字符。
	// 正规化为 ^[a-zA-Z0-9_-]+$，保证 LLM tool name "skill__<pluginName>__<skillSlug>" 合法。
	// 注：同样正规化 pluginName，防止上游传入含非法字符的插件名。
	skillSlug = mcp.SanitizeToolNamePart(skillSlug)
	safePluginName := mcp.SanitizeToolNamePart(pluginName)
	// 命名规范：skill:{safePluginName}__{skillSlug}
	// 使用 "__" 而非 "/"：LLM tool name 由 "skill__"+slug 构成，执行路径以 "skill:"+slug 反查 DB。
	fullName := "skill:" + safePluginName + "__" + skillSlug

	version := fm.Version
	caps := []string{}
	if fm.Capability != "" {
		caps = append(caps, "capability:"+fm.Capability)
	}

	meta := apptypes.SkillMeta{
		Name:            fullName,
		Version:         version,
		Runtime:         "script",
		RiskLevel:       fm.RiskLevel,
		Sandbox:         sandboxLevel(fm.Sandbox),
		Capabilities:    caps,
		ExecMode:        fm.ExecMode,
		AmbientPriority: fm.AmbientPriority,
		Trust:           apptypes.TrustTier(trustTier),
		Instructions:    string(data),
		PluginID:        pluginID,
	}

	if err := h.SkillReg.Register(ctx, meta); err != nil {
		slog.Warn("plugin_catalog: register skill failed", "skill", fullName, "err", err)
	} else if h.SyncSkillToToolRegistry != nil && fm.ExecMode == "tool" {
		slug := safePluginName + "__" + skillSlug
		h.SyncSkillToToolRegistry(slug, string(data))
	}
}

// safeJoin 将 rel 拼接到 base 下，并通过 EvalSymlinks + Rel 验证结果仍在 base 内。
// 防止 "../" 路径穿越；返回 (resolvedPath, true) 或 ("", false)。
func safeJoin(base, rel string) (string, bool) {
	resolvedBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		return "", false
	}
	// filepath.Clean("/" + rel) 将 rel 规范化为绝对形式再去掉前导 "/"，
	// 从而让 "../../etc/passwd" 变成 "/etc/passwd"，filepath.Join 后安全可比较。
	candidate := filepath.Join(resolvedBase, filepath.Clean("/"+rel))
	realCandidate, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		// 文件尚不存在时 EvalSymlinks 会报错；仅做静态检查
		realCandidate = candidate
	}
	relPart, err := filepath.Rel(resolvedBase, realCandidate)
	if err != nil || strings.HasPrefix(relPart, "..") || filepath.IsAbs(relPart) {
		return "", false
	}
	return realCandidate, true
}
