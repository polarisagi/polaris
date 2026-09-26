package marketplace

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/polarisagi/polaris/internal/extension/pluginspec"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// autoDependencyConfig 作为依赖自动安装的实例标记（extension_instances.config）。
const autoDependencyConfig = `{"auto_dependency":true}`

func isAutoDependency(inst types.ExtInstanceRow) bool {
	var cfg struct {
		Auto bool `json:"auto_dependency"`
	}
	return json.Unmarshal([]byte(inst.Config), &cfg) == nil && cfg.Auto
}

// dependencyConstraints 已安装插件对 name 声明的全部版本范围（依赖方插件名 → 范围）。
func (c *CatalogInstaller) dependencyConstraints(ctx context.Context, name string) (map[string]string, error) {
	rows, err := c.extRepo.ListPlugins(ctx)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeOf(err), "CatalogInstaller: plugins", err)
	}
	out := map[string]string{}
	for _, r := range rows {
		var spec pluginspec.Plugin
		if json.Unmarshal([]byte(r.Manifest), &spec) != nil {
			continue
		}
		for _, d := range spec.Dependencies {
			if d.Name == name {
				out[r.Name] = d.Version
			}
		}
	}
	return out, nil
}

func (c *CatalogInstaller) reresolveDependency(ctx context.Context, ic *installCtx, row *types.ExtCatalogRow, payload protocol.RegistryEntry,
	installed types.PluginRow, dep pluginspec.Dependency, key, requiredBy string,
) error {
	existing, err := c.dependencyConstraints(ctx, dep.Name)
	if err != nil {
		return err
	}
	ranges := []string{dep.Version}
	for _, v := range existing {
		if v != "" {
			ranges = append(ranges, v)
		}
	}
	combined := strings.Join(ranges, ", ")
	conflict := apperr.New(apperr.CodeConflict, fmt.Sprintf("Dependency %q (required by %s) has conflicting version requirements: %s (installed %s)",
		key, requiredBy, combined, installed.Version))
	var entry pluginspec.MarketplaceEntry
	if err := json.Unmarshal(payload.Entry, &entry); err != nil {
		return apperr.Wrap(apperr.CodeInvalidInput, "CatalogInstaller: entry "+payload.Name, err)
	}
	if src, ref, err := c.resolveVersion(ctx, row.MarketplaceID, entry, combined); err != nil || (ref == "" && src.Type != pluginspec.SourceNPM) {
		return conflict //nolint:nilerr // 无满足全部范围的版本即冲突
	}
	extID, err := c.instanceForRuntime(ctx, installed.ID)
	if err != nil {
		return err
	}
	return c.upgradeTo(ctx, ic, row, payload, combined, extID)
}

func (c *CatalogInstaller) instanceForRuntime(ctx context.Context, runtimeID string) (string, error) {
	insts, err := c.extRepo.ListInstances(ctx)
	if err != nil {
		return "", apperr.Wrap(apperr.CodeOf(err), "CatalogInstaller: instances", err)
	}
	for _, inst := range insts {
		if inst.RuntimeID == runtimeID {
			return inst.ID, nil
		}
	}
	return "", apperr.New(apperr.CodeNotFound, "no extension instance for plugin "+runtimeID)
}

// Prune 卸载作为依赖自动安装、且已没有任何已安装插件依赖的插件（Claude `plugin prune`）。
// 返回被卸载的目录条目 ID；卸载本身经 Manager.UninstallExtension 异步完成。
func (c *CatalogInstaller) Prune(ctx context.Context) ([]string, error) {
	insts, err := c.extRepo.ListInstances(ctx)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeOf(err), "CatalogInstaller.Prune", err)
	}
	plugins, err := c.extRepo.ListPlugins(ctx)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeOf(err), "CatalogInstaller.Prune", err)
	}
	needed := map[string]bool{}
	byRuntime := map[string]string{}
	for _, p := range plugins {
		byRuntime[p.ID] = p.Name
		var spec pluginspec.Plugin
		if json.Unmarshal([]byte(p.Manifest), &spec) == nil {
			for _, d := range spec.Dependencies {
				needed[d.Name] = true
			}
		}
	}
	var pruned []string
	for _, inst := range insts {
		name, isPlugin := byRuntime[inst.RuntimeID]
		if !isPlugin || !isAutoDependency(inst) || needed[name] || inst.CatalogID == "" || inst.Status == "uninstalling" {
			continue
		}
		if err := c.mgr.UninstallExtension(ctx, inst.CatalogID); err != nil {
			return pruned, apperr.Wrap(apperr.CodeOf(err), "CatalogInstaller.Prune: "+name, err)
		}
		pruned = append(pruned, inst.CatalogID)
	}
	return pruned, nil
}
