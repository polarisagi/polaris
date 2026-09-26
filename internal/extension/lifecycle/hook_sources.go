package lifecycle

import (
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"

	"github.com/polarisagi/polaris/internal/action/hook"
	"github.com/polarisagi/polaris/internal/extension/pluginspec"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// HookSourceProvider 组装 hooks.json 来源（实现 hook.SourceProvider，ADR-0103 决策六）：
// 用户级 <data>/hooks/hooks.json（已信任）、项目级 <dir>/.polaris/hooks/hooks.json 与已启用插件
// 的 hooks（均须按定义哈希审阅信任）。
type HookSourceProvider struct {
	extRepo     protocol.ExtensionRepository
	dataDir     string
	config      *PluginConfigService
	projectDirs func() []string // 当前项目目录（ADR-0097 项目模型）；nil = 无项目级来源
}

func NewHookSourceProvider(extRepo protocol.ExtensionRepository, dataDir string, config *PluginConfigService, projectDirs func() []string) *HookSourceProvider {
	return &HookSourceProvider{extRepo: extRepo, dataDir: dataDir, config: config, projectDirs: projectDirs}
}

// UserHooksPath 用户级 hooks.json 位置。
func UserHooksPath(dataDir string) string {
	return filepath.Join(dataDir, "hooks", "hooks.json")
}

func (p *HookSourceProvider) ListHookSources(ctx context.Context) ([]hook.Source, error) {
	trust, err := p.extRepo.ListHookTrust(ctx)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeOf(err), "HookSourceProvider", err)
	}
	var out []hook.Source
	if src, ok := p.fileSource(hook.ScopeUser, UserHooksPath(p.dataDir), trust); ok {
		out = append(out, src)
	}
	if p.projectDirs != nil {
		for _, dir := range p.projectDirs() {
			if src, ok := p.fileSource(hook.ScopeProject, filepath.Join(dir, ".polaris", "hooks", "hooks.json"), trust); ok {
				out = append(out, src)
			}
		}
	}
	plugins, err := p.pluginSources(ctx, trust)
	if err != nil {
		return nil, err
	}
	return append(out, plugins...), nil
}

// fileSource 解析失败只跳过该来源并留痕：一份坏掉的 hooks.json 不能让所有 hook 失效。
func (p *HookSourceProvider) fileSource(scope hook.Scope, path string, trust map[string]string) (hook.Source, bool) {
	src, ok, err := hook.FileSource(scope, path)
	if err != nil {
		slog.Warn("hook: invalid hooks.json skipped", "path", path, "err", err)
		return hook.Source{}, false
	}
	if ok && scope != hook.ScopeUser {
		src.Trusted = trust[src.Key] == src.Digest
	}
	return src, ok
}

func (p *HookSourceProvider) pluginSources(ctx context.Context, trust map[string]string) ([]hook.Source, error) {
	plugins, err := p.extRepo.ListPlugins(ctx)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeOf(err), "HookSourceProvider.plugins", err)
	}
	var out []hook.Source
	for _, row := range plugins {
		if !row.Enabled {
			continue
		}
		var spec pluginspec.Plugin
		if err := json.Unmarshal([]byte(row.Manifest), &spec); err != nil {
			slog.Warn("hook: corrupt plugin manifest, hooks skipped", "plugin", row.ID, "err", err)
			continue
		}
		options := p.pluginOptions(ctx, row.ID)
		for _, hs := range spec.Hooks {
			cfg, err := hook.ParseEvents(hs.Events)
			if err != nil {
				slog.Warn("hook: invalid plugin hooks skipped", "plugin", row.ID, "source", hs.Source, "err", err)
				continue
			}
			key := PluginHookKey(row.ID, row.InstallPath, hs.Source)
			out = append(out, hook.Source{Key: key, Scope: hook.ScopePlugin, PluginID: row.ID, PluginName: row.Name,
				PluginRoot: row.InstallPath, PluginData: PluginDataDir(p.dataDir, row.ID), Options: options,
				Digest: hs.Digest, Trusted: trust[key] == hs.Digest, Config: cfg})
		}
	}
	return out, nil
}

// pluginOptions 敏感值也导出给 hook 进程（Claude：CLAUDE_PLUGIN_OPTION_<KEY> 覆盖全部选项）；
// 必填缺失不阻断 hook 装载。
func (p *HookSourceProvider) pluginOptions(ctx context.Context, pluginID string) map[string]string {
	if p.config == nil {
		return nil
	}
	values, _, err := p.config.ResolveStrings(ctx, pluginID, "", true)
	if err != nil {
		slog.Warn("hook: resolve plugin options failed", "plugin", pluginID, "err", err)
		return nil
	}
	return values
}

// PluginHookKey 信任键使用插件内相对路径，避免安装目录变化导致信任失效。
func PluginHookKey(pluginID, root, source string) string {
	if rel, err := filepath.Rel(root, source); err == nil && !filepath.IsAbs(rel) {
		source = filepath.ToSlash(rel)
	}
	return "plugin:" + pluginID + ":" + source
}
