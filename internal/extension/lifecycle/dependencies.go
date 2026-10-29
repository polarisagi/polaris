package lifecycle

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	semver "github.com/Masterminds/semver/v3"

	"github.com/polarisagi/polaris/internal/extension/pluginspec"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// 依赖状态（Claude plugin dependencies：依赖须已安装、已启用且版本满足范围，否则依赖方不加载）。
const (
	DepOK              = "ok"
	DepMissing         = "missing"
	DepDisabled        = "disabled"
	DepVersionMismatch = "version_mismatch"
	DepBadConstraint   = "invalid_constraint"
)

// DependencyStatus 单条依赖的评估结果。
type DependencyStatus struct {
	Name             string `json:"name"`
	Marketplace      string `json:"marketplace,omitempty"`
	Constraint       string `json:"constraint,omitempty"`
	PluginID         string `json:"plugin_id,omitempty"`
	InstalledVersion string `json:"installed_version,omitempty"`
	State            string `json:"state"`
	Message          string `json:"message,omitempty"`
}

// PluginDependencies 插件依赖的加载期检查（ADR-0103 决策三）。安装与版本解析随市场来源实现
// （决策七）；本服务只判定"当前已安装集合能否满足"，并在不满足时阻止启用 / 级联停用。
type PluginDependencies struct {
	extRepo protocol.ExtensionRepository
	servers ServerStopper // 可为 nil（启动期尚无连接）
}

// ServerStopper 停用插件时断开其子 MCP（实现为 *mcp.MCPManager）。
type ServerStopper interface {
	Remove(serverID string)
}

func NewPluginDependencies(extRepo protocol.ExtensionRepository, servers ServerStopper) *PluginDependencies {
	return &PluginDependencies{extRepo: extRepo, servers: servers}
}

type pluginIndex struct {
	byID   map[string]types.PluginRow
	byName map[string]types.PluginRow
	specs  map[string]pluginspec.Plugin
}

func (d *PluginDependencies) index(ctx context.Context) (pluginIndex, error) {
	rows, err := d.extRepo.ListPlugins(ctx)
	if err != nil {
		return pluginIndex{}, apperr.Wrap(apperr.CodeOf(err), "PluginDependencies", err)
	}
	idx := pluginIndex{byID: map[string]types.PluginRow{}, byName: map[string]types.PluginRow{}, specs: map[string]pluginspec.Plugin{}}
	for _, row := range rows {
		idx.byID[row.ID], idx.byName[row.Name] = row, row
		var spec pluginspec.Plugin
		if err := json.Unmarshal([]byte(row.Manifest), &spec); err != nil {
			slog.Warn("plugin deps: corrupt manifest", "plugin", row.ID, "err", err)
			continue
		}
		idx.specs[row.ID] = spec
	}
	return idx, nil
}

// Check 评估插件的全部依赖。依赖按名称匹配已安装插件（插件名全局唯一）。
func (d *PluginDependencies) Check(ctx context.Context, pluginID string) ([]DependencyStatus, error) {
	idx, err := d.index(ctx)
	if err != nil {
		return nil, err
	}
	return evaluateDeps(idx, idx.specs[pluginID].Dependencies), nil
}

func evaluateDeps(idx pluginIndex, deps []pluginspec.Dependency) []DependencyStatus {
	out := make([]DependencyStatus, 0, len(deps))
	for _, dep := range deps {
		st := DependencyStatus{Name: dep.Name, Marketplace: dep.Marketplace, Constraint: dep.Version, State: DepOK}
		row, ok := idx.byName[dep.Name]
		switch {
		case !ok:
			st.State, st.Message = DepMissing, fmt.Sprintf("Dependency %q is not installed", dep.Name)
		case !row.Enabled:
			st.PluginID, st.InstalledVersion = row.ID, row.Version
			st.State, st.Message = DepDisabled, fmt.Sprintf("Dependency %q is disabled — enable it or remove the dependency", dep.Name)
		default:
			st.PluginID, st.InstalledVersion = row.ID, row.Version
			st.State, st.Message = versionState(dep, row.Version)
		}
		out = append(out, st)
	}
	return out
}

// versionState npm semver 范围（^ ~ >= = || 连字符）；范围不匹配预发布版本，除非范围自身带预发布后缀。
func versionState(dep pluginspec.Dependency, installed string) (string, string) {
	if strings.TrimSpace(dep.Version) == "" {
		return DepOK, ""
	}
	c, err := semver.NewConstraint(dep.Version)
	if err != nil {
		return DepBadConstraint, fmt.Sprintf("Dependency %q has invalid version range %q", dep.Name, dep.Version)
	}
	v, err := semver.NewVersion(installed)
	if err != nil || !c.Check(v) {
		return DepVersionMismatch, fmt.Sprintf("Requires %q %s, installed %s", dep.Name, dep.Version, installed)
	}
	return DepOK, ""
}

// EnableBlocker 启用前校验：任一依赖不满足即拒绝（CodeConflict），错误信息列出全部问题。
func (d *PluginDependencies) EnableBlocker(ctx context.Context, pluginID string) error {
	statuses, err := d.Check(ctx, pluginID)
	if err != nil {
		return err
	}
	var problems []string
	for _, st := range statuses {
		if st.State != DepOK {
			problems = append(problems, st.Message)
		}
	}
	if len(problems) > 0 {
		return apperr.New(apperr.CodeConflict, "plugin dependencies not satisfied: "+strings.Join(problems, "; "))
	}
	return nil
}

// EnabledDependents 依赖 pluginID 的已启用插件（停用/卸载依赖时须级联停用它们）。
func (d *PluginDependencies) EnabledDependents(ctx context.Context, pluginID string) ([]types.PluginRow, error) {
	idx, err := d.index(ctx)
	if err != nil {
		return nil, err
	}
	target, ok := idx.byID[pluginID]
	if !ok {
		return nil, nil
	}
	var out []types.PluginRow
	for id, spec := range idx.specs {
		row := idx.byID[id]
		if id == pluginID || !row.Enabled {
			continue
		}
		for _, dep := range spec.Dependencies {
			if dep.Name == target.Name {
				out = append(out, row)
				break
			}
		}
	}
	return out, nil
}

// EnforceAll 加载期检查（启动、停用或卸载某插件之后）：已启用但依赖不满足的插件置为停用（Claude：依赖不满足的插件在下次
// 加载时被停用并报错）。须在恢复 MCP 连接之前调用；迭代至不动点以处理依赖链。
func (d *PluginDependencies) EnforceAll(ctx context.Context) ([]string, error) {
	var disabled []string
	for {
		idx, err := d.index(ctx)
		if err != nil {
			return disabled, err
		}
		changed := false
		for id, spec := range idx.specs {
			row := idx.byID[id]
			if !row.Enabled || allOK(evaluateDeps(idx, spec.Dependencies)) {
				continue
			}
			if err := d.disable(ctx, id); err != nil {
				return disabled, err
			}
			disabled, changed = append(disabled, id), true
		}
		if !changed {
			return disabled, nil
		}
	}
}

func (d *PluginDependencies) disable(ctx context.Context, pluginID string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	row, err := d.pluginRow(ctx, pluginID)
	if err != nil {
		return err
	}
	if err := d.extRepo.UpdatePluginStatus(ctx, pluginID, 0, row.MCPPolicy, now); err != nil {
		return apperr.Wrap(apperr.CodeOf(err), "PluginDependencies.disable", err)
	}
	if err := d.extRepo.SetPluginComponentsEnabled(ctx, pluginID, 0, now); err != nil {
		return apperr.Wrap(apperr.CodeOf(err), "PluginDependencies.disable components", err)
	}
	if d.servers != nil {
		servers, err := d.extRepo.ListMCPServers(ctx)
		if err != nil {
			return apperr.Wrap(apperr.CodeOf(err), "PluginDependencies.disable servers", err)
		}
		for _, srv := range servers {
			if srv.PluginID == pluginID {
				d.servers.Remove(srv.ID)
			}
		}
	}
	slog.Warn("plugin deps: plugin disabled, dependencies not satisfied", "plugin", pluginID)
	return nil
}

func (d *PluginDependencies) pluginRow(ctx context.Context, pluginID string) (types.PluginRow, error) {
	idx, err := d.index(ctx)
	if err != nil {
		return types.PluginRow{}, err
	}
	row, ok := idx.byID[pluginID]
	if !ok {
		return types.PluginRow{}, apperr.New(apperr.CodeNotFound, "plugin not found: "+pluginID)
	}
	return row, nil
}

func allOK(statuses []DependencyStatus) bool {
	for _, st := range statuses {
		if st.State != DepOK {
			return false
		}
	}
	return true
}
