package lifecycle

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/extension/pluginspec"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
	"github.com/polarisagi/polaris/pkg/types"
)

// MCPConnector 按 mcp_servers 行启动连接（MCPManager.StartFromDB）。行是配置权威源，
// 安装层只写行，不再自行拼装客户端配置。
type MCPConnector interface {
	StartFromDB(ctx context.Context, serverID string) error
	GetClient(serverID string) protocol.MCPClient
	Remove(serverID string)
}

// PluginInstaller 插件唯一安装实现（ADR-0103 决策二/三）：经 pluginspec 解析三种清单格式，
// 写 plugins / skills / mcp_servers，并异步启动子 MCP。组件级失败只记入诊断，不中断其余组件。
type PluginInstaller struct {
	extRepo  protocol.ExtensionRepository
	mcpConn  MCPConnector
	skillReg protocol.SkillRegistry
	// policyGate 对插件内嵌子 MCP 逐个独立授权（extension/CLAUDE.md 硬约束 3，
	// GR-8-002）。父插件的安装授权不能代替子 MCP：子 MCP 会拉起任意本地进程
	// 并对 Agent 暴露工具，风险面与插件本身不同。nil 时 fail-closed 跳过全部子 MCP。
	policyGate protocol.PolicyGate
	// dataDir 用于卸载时删除 ${PLUGIN_DATA}（升级保留、卸载删除，两家同语义）。
	dataDir string
}

// WithDataDir 注入数据根目录。
func (p *PluginInstaller) WithDataDir(dir string) *PluginInstaller {
	p.dataDir = dir
	return p
}

func NewPluginInstaller(extRepo protocol.ExtensionRepository, mcpConn MCPConnector, skillReg protocol.SkillRegistry) *PluginInstaller {
	return &PluginInstaller{extRepo: extRepo, mcpConn: mcpConn, skillReg: skillReg}
}

// WithPolicyGate 注入子 MCP 授权网关。
func (p *PluginInstaller) WithPolicyGate(pg protocol.PolicyGate) *PluginInstaller {
	p.policyGate = pg
	return p
}

func (p *PluginInstaller) ExtType() types.ExtType { return types.TypePlugin }

// PluginRuntimeID 插件运行时 ID（plugins.id）："pl_" + 实例 ID 后缀。
func PluginRuntimeID(instID string) string {
	return "pl_" + strings.TrimPrefix(instID, "ext_")
}

func (p *PluginInstaller) Install(ctx context.Context, req InstallReq) (InstallResult, error) {
	if req.LocalPath == "" {
		return InstallResult{}, apperr.New(apperr.CodeInvalidInput, "plugin_installer: LocalPath required")
	}
	plug, err := pluginspec.Load(req.LocalPath, pluginspec.LoadOptions{FallbackName: req.Name})
	if err != nil {
		return InstallResult{}, apperr.Wrap(apperr.CodeOf(err), "plugin_installer: load", err)
	}
	pluginID := PluginRuntimeID(req.InstID)
	// 升级 / 重装：先清掉上一版本的子组件，新版本已删除的技能与服务器不得残留。
	if err := p.resetPreviousComponents(ctx, pluginID); err != nil {
		return InstallResult{}, err
	}
	p.registerSkills(ctx, req, pluginID, plug)
	var serverIDs []string
	for _, srv := range plug.MCPServers {
		if id, ok := p.registerBundleMCP(ctx, req, pluginID, plug, srv); ok {
			serverIDs = append(serverIDs, id)
		}
	}
	if err := p.savePlugin(ctx, req, pluginID, plug); err != nil {
		return InstallResult{}, err
	}
	if plug.DefaultEnabled {
		p.startServers(serverIDs)
	}
	for _, d := range plug.Diagnostics {
		slog.Warn("plugin_installer: diagnostic", "plugin", plug.Name, "diag", d.String())
	}
	return InstallResult{Dir: req.LocalPath, RuntimeID: pluginID}, nil
}

func (p *PluginInstaller) Uninstall(ctx context.Context, req UninstallReq) error {
	pluginID := req.RuntimeID
	if pluginID == "" {
		pluginID = PluginRuntimeID(req.InstID) // runtime_id 回写修复前安装的实例
	}
	if err := p.resetPreviousComponents(ctx, pluginID); err != nil {
		return apperr.Wrap(apperr.CodeOf(err), "plugin_installer.Uninstall", err)
	}
	if p.dataDir != "" {
		if err := os.RemoveAll(PluginDataDir(p.dataDir, pluginID)); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "plugin_installer.Uninstall: remove plugin data", err)
		}
	}
	return nil
}

func (p *PluginInstaller) resetPreviousComponents(ctx context.Context, pluginID string) error {
	servers, err := p.extRepo.ListMCPServers(ctx)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "plugin_installer: list servers", err)
	}
	for _, s := range servers {
		if s.PluginID == pluginID && p.mcpConn != nil {
			p.mcpConn.Remove(s.ID)
		}
	}
	if err := p.extRepo.UninstallCleanup(ctx, pluginID, "", string(types.TypePlugin)); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "plugin_installer: reset components", err)
	}
	return nil
}

// savePlugin 写 plugins 行；manifest 列保存归一化模型快照（含诊断、不适用组件、应用绑定），
// 供 UI 展示与后续阶段（hooks 信任、用户配置、Agent 映射）读取。
func (p *PluginInstaller) savePlugin(ctx context.Context, req InstallReq, pluginID string, plug *pluginspec.Plugin) error {
	snapshot, err := json.Marshal(plug)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "plugin_installer: marshal manifest", err)
	}
	publisher := req.Publisher
	if plug.Author != nil && plug.Author.Name != "" {
		publisher = plug.Author.Name
	}
	now := time.Now().UTC().Format(time.RFC3339)
	row := types.PluginRow{
		ID: pluginID, Name: plug.Name, Version: firstNonEmpty(plug.Version, "0.0.0"),
		DisplayName: firstNonEmpty(plug.DisplayName, plug.Name), Description: plug.Description,
		Publisher: publisher, Homepage: plug.Homepage, InstallPath: plug.Root, Enabled: plug.DefaultEnabled,
		TrustTier: req.TrustTier, CatalogID: req.CatalogID, MCPPolicy: "{}", Manifest: string(snapshot),
		CreatedAt: now, UpdatedAt: now,
	}
	if err := p.extRepo.UpsertPlugin(ctx, row); err != nil {
		return apperr.Wrap(apperr.CodeOf(err), "plugin_installer: save plugin", err)
	}
	return nil
}

func (p *PluginInstaller) registerSkills(ctx context.Context, req InstallReq, pluginID string, plug *pluginspec.Plugin) {
	if p.skillReg == nil {
		// Tier-0 等未装配 SkillRegistry 的形态：技能不可用须留痕，不能让插件看起来完整安装。
		if len(plug.Skills) > 0 {
			slog.Error("plugin_installer: skill registry unavailable, plugin skills not registered", "plugin", plug.Name)
		}
		return
	}
	for _, s := range plug.Skills {
		meta := skillMetaFromSpec(s, PluginSkillName(plug.Name, s.Name), plug.Version, pluginID, types.TrustTier(req.TrustTier))
		if !plug.DefaultEnabled {
			meta.Deprecated = true // 与插件停用级联语义一致：停用插件的技能不对模型可见
		}
		if err := p.skillReg.Register(ctx, meta); err != nil {
			slog.Warn("plugin_installer: register skill failed", "plugin", plug.Name, "skill", s.Name, "err", err)
			plug.Diagnostics = append(plug.Diagnostics, pluginspec.Diagnostic{Severity: pluginspec.SeverityError,
				Component: string(s.Kind), Path: s.File, Rule: "polaris.skill.register", Message: err.Error()})
		}
	}
}

// registerBundleMCP 注册插件内嵌的单个子 MCP（先独立授权，拒绝则 skip）。
func (p *PluginInstaller) registerBundleMCP(ctx context.Context, req InstallReq, pluginID string, plug *pluginspec.Plugin, srv pluginspec.MCPServer) (string, bool) {
	serverID := "plugin_" + pluginID + "_" + srv.Name
	target := firstNonEmpty(srv.Command, srv.URL)
	if !p.authorizeBundleMCP(ctx, req, serverID, target) {
		plug.Diagnostics = append(plug.Diagnostics, pluginspec.Diagnostic{Severity: pluginspec.SeverityError,
			Component: "mcp", Path: srv.Source, Rule: "polaris.mcp.policy", Message: "server " + srv.Name + " denied by policy"})
		return "", false
	}
	row := mcpRowFromSpec(srv, mcpRowParams{ID: serverID, Name: scopedServerName(plug.Name, srv.Name),
		PluginID: pluginID, TrustTier: req.TrustTier, Enabled: plug.DefaultEnabled})
	if err := p.extRepo.UpsertMCPServer(ctx, row); err != nil {
		// L2：子 MCP 持久化失败不中断父插件安装（硬约束 3），但不能再启动一个
		// 重启后无法恢复的进程；留痕并跳过。
		slog.Warn("plugin_installer: persist bundle mcp failed, skipped", "plugin", req.InstID, "server", serverID, "err", err)
		return "", false
	}
	return serverID, true
}

// scopedServerName 同一插件多服务器时用 "插件-服务器" 区分；服务器名已是插件名后缀时不重复拼接。
func scopedServerName(pluginName, serverName string) string {
	if pluginName == serverName || strings.HasSuffix(pluginName, "-"+serverName) {
		return pluginName
	}
	return pluginName + "-" + serverName
}

// startServers 插件行落库后再启动：ConfigFromRow 需按 plugin_id 读取插件根展开变量。
func (p *PluginInstaller) startServers(serverIDs []string) {
	if p.mcpConn == nil {
		return
	}
	for _, id := range serverIDs {
		concurrent.SafeGo(context.Background(), "plugin_installer.mcp_start", func(ctx context.Context) {
			ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
			defer cancel()
			if err := p.mcpConn.StartFromDB(ctx, id); err != nil {
				slog.Warn("plugin_installer: start bundle mcp failed", "server", id, "err", err)
			}
		})
	}
}

// authorizeBundleMCP 对单个内嵌子 MCP 调用 PolicyGate；拒绝或出错返回 false。
func (p *PluginInstaller) authorizeBundleMCP(ctx context.Context, req InstallReq, serverID, target string) bool {
	if p.policyGate == nil {
		slog.Warn("plugin_installer: no policy gate, bundle mcp skipped (fail-closed)", "plugin", req.InstID, "server", serverID)
		return false
	}
	res, err := p.policyGate.Review(ctx, types.PolicyReviewRequest{
		Principal: "plugin:" + req.InstID,
		Action:    "install_extension",
		Resource:  serverID,
		Context: map[string]any{
			"trust_level":   req.TrustTier,
			"publisher":     req.Publisher,
			"ext_type":      string(types.TypeMCP),
			"bundle_parent": req.InstID,
			"command":       target,
		},
	})
	if err != nil || !res.Allowed {
		slog.Warn("plugin_installer: bundle mcp denied by policy, skipped",
			"plugin", req.InstID, "server", serverID, "reason", res.Reason, "err", err)
		return false
	}
	return true
}
