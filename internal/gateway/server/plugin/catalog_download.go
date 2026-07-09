package plugin

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/polarisagi/polaris/internal/extension/marketplace"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	apptypes "github.com/polarisagi/polaris/pkg/types"
)

// ============================================================================
// skill/plugin 扩展的异步下载/安装 + 目录拷贝辅助（R7 拆分自 catalog_install.go）。
// MCP/generic 安装 HTTP 处理器见 catalog_install.go。
// ============================================================================

// downloadAndInstallExtension 把 skill/plugin 类扩展从本地 marketplace 缓存目录拷贝到
// extensions/{extID} 运行时目录；失败时把错误写回 extension_instances，供前端轮询安装状态。
//
//nolint:nestif
func (h *PluginHandler) downloadAndInstallExtension(ctx context.Context, extID, catalogID string, entry *protocol.RegistryEntry, now, name string) { //nolint:gocyclo,nestif
	// 1. 获取本地 tmp 目录路径
	// marketplace_id 本身可含 "/"（如 "polarisagi/polaris-plugins-official"），
	// 不能在第一个 "/" 处分割，必须从 extension_catalog 读取准确值。
	var mpID string
	if err := h.DB.QueryRowContext(ctx,
		`SELECT marketplace_id FROM extension_catalog WHERE id=?`, catalogID).Scan(&mpID); err != nil {
		h.updateExtensionInstanceError(ctx, extID, "catalog entry not found: "+err.Error())
		return
	}
	relPath := filepath.FromSlash(strings.TrimPrefix(catalogID, mpID+"/"))

	safeMpID := strings.ReplaceAll(mpID, "/", "_")
	srcDir := filepath.Join(h.DataDir, "tmp", "marketplaces", safeMpID, relPath)
	destDir := filepath.Join(h.DataDir, "extensions", extID)

	// 2. 拷贝目录
	if err := os.MkdirAll(filepath.Dir(destDir), 0755); err != nil {
		h.updateExtensionInstanceError(ctx, extID, err.Error())
		return
	}
	if err := copyDir(srcDir, destDir); err != nil {
		// 回退尝试 git sparse checkout 或者直接报错。当前假定 sync 已经拉取好了全量。
		h.updateExtensionInstanceError(ctx, extID, "failed to copy from tmp: "+err.Error())
		return
	}

	runtimeID := ""
	// 3. 路由到对应的运行时表
	if entry.Type == "skill" {
		// 命名规范：skill:{hex}，与 SkillRegistry 的 "skill:" 前缀约束对齐，同时保持全局唯一
		runtimeID = "skill:" + extID[4:]
		if h.SkillReg == nil {
			h.updateExtensionInstanceError(ctx, extID, "skill registry not available")
			return
		}
		skillMDBytes, err := os.ReadFile(filepath.Join(destDir, "SKILL.md"))
		if err != nil {
			h.updateExtensionInstanceError(ctx, extID, "SKILL.md not found")
			return
		}
		fm := parseFrontmatter(string(skillMDBytes))
		if fm.Description == "" {
			fm.Description = entry.Description
		}
		caps := []string{"description:" + fm.Description}
		if fm.Capability != "" {
			caps = append(caps, "capability:"+fm.Capability)
		}
		meta := apptypes.SkillMeta{
			Name:         runtimeID,
			Version:      fm.Version,
			Runtime:      "script",
			RiskLevel:    fm.RiskLevel,
			Sandbox:      sandboxLevel(fm.Sandbox),
			Capabilities: caps,
			ExecMode:     fm.ExecMode,
			Trust:        apptypes.TrustTier(entry.TrustTier),
			Instructions: string(skillMDBytes),
			PluginID:     "", // 独立安装，无插件归属
		}
		if err := h.SkillReg.Register(ctx, meta); err != nil {
			h.updateExtensionInstanceError(ctx, extID, "register skill: "+err.Error())
			return
		}
	} else if entry.Type == "plugin" {
		runtimeID = "pl_" + extID[4:]

		// 解析 Bundle 清单（Polaris 原生格式优先；权威来源是文件系统，DB 只做快照缓存）
		var bundle protocol.PluginBundleManifest
		var manifestRaw []byte
		for _, manifestPath := range []string{
			filepath.Join(destDir, ".polaris-plugin", "plugin.json"),
			filepath.Join(destDir, "plugin.json"),
		} {
			if raw, err2 := os.ReadFile(manifestPath); err2 == nil {
				manifestRaw = raw
				_ = json.Unmarshal(raw, &bundle)
				break
			}
		}
		if manifestRaw == nil {
			manifestRaw = []byte("{}")
		}

		// 收集插件内所有 MCP 服务器定义（三种来源），写入 mcp_servers 表并构建 mcp_policy。
		// mcp_policy 仅存储额外策略（approval_mode、enabled_tools 等），enabled 状态由 mcp_servers.enabled 权威管理。
		mcpPolicyMap := make(map[string]map[string]any)
		allMCPs := make(map[string]pluginMCPDef)

		for srvName, def := range bundle.MCPInline {
			allMCPs[srvName] = pluginMCPDef{Command: def.Command, Args: def.Args, Env: def.Env, URL: def.URL}
			mcpPolicyMap[srvName] = map[string]any{}
		}
		if bundle.MCPFile != "" {
			if safePath, ok := safeJoin(destDir, bundle.MCPFile); ok {
				if mcpCfg, err2 := marketplace.GetMCPConfig(safePath); err2 == nil {
					for srvName, def := range mcpCfg.MCPServers {
						if _, exists := allMCPs[srvName]; !exists {
							allMCPs[srvName] = pluginMCPDef{Command: def.Command, Args: def.Args, Env: def.Env, URL: def.URL}
							mcpPolicyMap[srvName] = map[string]any{}
						}
					}
				}
			}
		}
		// 兼容第三方格式（OpenAI ai-plugin.json / Anthropic plugin.toml 等）
		if subEntries, err2 := marketplace.ParseManifestDir(destDir, "", protocol.Marketplace{
			ID: "bundle_" + extID, Publisher: entry.Publisher, TrustTier: entry.TrustTier,
		}); err2 == nil {
			for _, sub := range subEntries {
				if sub.Type == "mcp" && sub.Command != "" {
					if _, exists := allMCPs[sub.Name]; !exists {
						allMCPs[sub.Name] = pluginMCPDef{Command: sub.Command, Args: sub.Args, URL: sub.URL}
						mcpPolicyMap[sub.Name] = map[string]any{}
					}
				}
			}
		}
		mcpPolicyBytes, _ := json.Marshal(mcpPolicyMap)

		// 从 bundle interface 字段提取展示信息
		displayName := name
		homepage := ""
		if bundle.Interface != nil {
			if bundle.Interface.DisplayName != "" {
				displayName = bundle.Interface.DisplayName
			}
			homepage = bundle.Interface.WebsiteURL
		}

		err := h.ExtRepo.UpsertPlugin(ctx, runtimeID, name, bundle.Version, displayName, entry.Description,
			entry.Publisher, homepage, destDir, 1, entry.TrustTier, catalogID, string(mcpPolicyBytes), string(manifestRaw), now, now)
		if err != nil {
			h.updateExtensionInstanceError(ctx, extID, "insert plugin err: "+err.Error())
			return
		}

		// 注册插件内置的 skills（agentskills.io 标准：skills 是插件 bundle 的一等组件）
		h.registerPluginSkills(ctx, runtimeID, name, destDir, &bundle, entry.TrustTier)

		// 将插件内嵌的 MCP 写入 mcp_servers 表并异步启动（统一架构，State-in-DB）
		h.registerPluginMCPServers(ctx, runtimeID, name, destDir, allMCPs, entry.TrustTier, now)

		if hook, ok := bundle.Hooks["install"]; ok && hook != "" {
			if hookPath, ok := safeJoin(destDir, hook); ok {
				if h.ScriptRunner != nil {
					// ContainerSandbox.RunHook：Linux 下有 PID/NS namespace 隔离
					if err := h.ScriptRunner.RunHook(ctx, hookPath, destDir); err != nil {
						slog.Warn("plugin_catalog: install hook failed", "ext", extID, "err", err)
					}
				} else {
					// scriptRunner 未注入（如 Tier-0 macOS 无 L3）：skip，记录警告
					slog.Warn("plugin_catalog: install hook skipped (no scriptRunner, call SetScriptRunner to enable)",
						"ext", extID, "hook", hookPath)
				}
			}
		}
	}

	// 4. 更新 extension_instances 为 installed
	_ = h.InstallMgr.UpdateInstance(ctx, extID, marketplace.InstanceUpdate{
		Status:      "installed",
		RuntimeID:   runtimeID,
		InstallPath: destDir,
		ClearError:  true,
	})
}

func (h *PluginHandler) updateExtensionInstanceError(ctx context.Context, extID, errMsg string) {
	if h.InstallMgr != nil {
		_ = h.InstallMgr.UpdateInstance(ctx, extID, marketplace.InstanceUpdate{
			Status:   "error",
			ErrorMsg: errMsg,
		})
	}
}

func copyDir(src string, dst string) error {
	srcInfo, err := os.Stat(src)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "copyDir", err)
	}
	if err := os.MkdirAll(dst, srcInfo.Mode()); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "copyDir", err)
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "copyDir", err)
	}
	for _, entry := range entries {
		srcPath := filepath.Join(src, entry.Name())
		dstPath := filepath.Join(dst, entry.Name())
		if entry.IsDir() {
			if err := copyDir(srcPath, dstPath); err != nil {
				return apperr.Wrap(apperr.CodeInternal, "copyDir", err)
			}
		} else {
			if err := copyFile(srcPath, dstPath); err != nil {
				return apperr.Wrap(apperr.CodeInternal, "copyDir", err)
			}
		}
	}
	return nil
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "copyFile", err)
	}
	info, err := os.Stat(src)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "copyFile", err)
	}
	return os.WriteFile(dst, data, info.Mode()) //nolint:wrapcheck
}

// pluginMCPDef 是 registerPluginMCPServers 的内部传参结构，避免与 protocol 包产生循环依赖。
type pluginMCPDef struct {
	Command string
	Args    []string
	Env     map[string]string
	URL     string
}
