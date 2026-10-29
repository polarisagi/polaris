package agent

// 工作区上下文与项目作用域（GD-14-005 / ADR-0097）：每轮在"无 effect 运行"的窗口
// 解析会话所属项目，驱动两件事——上下文文档装载（refreshWorkspaceContext）与
// 工具文件访问根（projectRoot → withTaskScopeCtx）。自 agent_lifecycle.go 拆出（R7 行数上限）。

import (
	"context"
	"path/filepath"

	agentctx "github.com/polarisagi/polaris/internal/agent/context"
	protorepo "github.com/polarisagi/polaris/internal/protocol/repo"
)

// refreshWorkspaceContext 探测并装载工作区标准上下文文档（GD-14-005）。
//
// 与 refreshInstalledExtensions 并列在感知阶段刷新，而非启动时装载一次：
// 工作区在会话过程中可能被切换（VFS GetRootDir 变化），且用户可能在会话中
// 修改 AGENTS.md——每轮重读的成本是几次 stat + 小文件读，远低于装载过期约束
// 带来的行为不一致。
//
// 信任边界在 loader 内部判定（见 WorkspaceContextLoader.isTrusted）：
// 未在配置中显式声明信任的工作区，其上下文一律走 Untrusted 通道。
func (a *Agent) refreshWorkspaceContext(ctx context.Context) {
	pc := a.resolveProject(ctx)
	a.projectRoot.Store(scopeRootOf(pc))
	projectID := protorepo.DefaultProjectID
	if pc != nil && pc.ID != "" {
		projectID = pc.ID
	}
	a.projectID.Store(projectID)
	a.sCtx.Mu.Lock()
	a.sCtx.ProjectID = projectID
	a.sCtx.Mu.Unlock()
	if a.workspaceCtxLoader == nil {
		return
	}
	// 项目绑定了目录 → 以项目目录为准；否则沿用装配时的工作区根（每任务沙箱根，
	// 默认项目 / 无目录项目 / agent-0 的既有行为不变）。
	var docs []agentctx.WorkspaceContext
	switch {
	case pc != nil && pc.Root != "":
		docs = a.workspaceCtxLoader.ListForProject(ctx, *pc)
	default:
		if a.workspaceRoot != "" {
			docs = a.workspaceCtxLoader.Load(ctx, a.workspaceRoot)
		}
		if pc != nil {
			docs = append(docs, a.workspaceCtxLoader.ListForProject(ctx, agentctx.ProjectContext{Instructions: pc.Instructions})...)
		}
	}
	if a.workspaceRoot == "" && pc == nil {
		return // 既无工作区根也无项目：无可装载来源，不必加锁刷新
	}

	a.sCtx.Mu.Lock()
	a.sCtx.WorkspaceContextTrusted = agentctx.RenderTrusted(docs)
	if ts := agentctx.RenderUntrusted(docs); !ts.IsEmpty() {
		a.sCtx.WorkspaceContextUntrusted = ts.UnsafeContent()
	} else {
		a.sCtx.WorkspaceContextUntrusted = ""
	}
	a.sCtx.Mu.Unlock()
}

// resolveProject 解析本会话所属项目：先按会话，解析不到再按共享记忆命名空间所指会话
// （ADR-0097 决策三补：委派子 Agent 的会话 ID 是一次性的，命名空间即发起方根会话 ID）。
func (a *Agent) resolveProject(ctx context.Context) *agentctx.ProjectContext {
	if a.projectResolver == nil {
		return nil
	}
	sessionID := a.sCtx.SessionID
	if pc := a.projectResolver(ctx, sessionID); pc != nil {
		return pc
	}
	if ns := a.memoryNamespace(); ns != "" && ns != sessionID {
		return a.projectResolver(ctx, ns)
	}
	return nil
}

// memoryNamespace 共享记忆命名空间的原子副本：SetMemoryNamespace 由 Worker 在 Run()
// 之外的 goroutine 调用，而本值在 Run() 循环里读取。
func (a *Agent) memoryNamespace() string {
	ns, _ := a.projectNamespace.Load().(string)
	return ns
}

// currentProjectID 本会话当前所属项目；未解析过或无项目时为默认项目。
func (a *Agent) currentProjectID() string {
	if id, _ := a.projectID.Load().(string); id != "" {
		return id
	}
	return protorepo.DefaultProjectID
}

// scopeRootOf 项目工作目录能否作为本会话工具的文件访问根（ADR-0097 决策五）。
// 与 ListForProject 的信任撤销同一判据：存库的是规范路径，若它现在解析到别处
// （目录被替换成软链），不授予访问——否则替换软链即可把访问面挪到任意目录。
// 敏感目录判据在工具侧 guard.ScopedPaths 再校验一次。
func scopeRootOf(pc *agentctx.ProjectContext) string {
	if pc == nil || pc.Root == "" {
		return ""
	}
	resolved, err := filepath.EvalSymlinks(pc.Root)
	if err != nil || resolved != pc.Root {
		return ""
	}
	return pc.Root
}

// currentProjectRoot 本会话当前的项目工作目录；无则空串。
func (a *Agent) currentProjectRoot() string {
	root, _ := a.projectRoot.Load().(string)
	return root
}

// InjectWorkspaceContextLoader 注入工作区上下文装载器与工作区根目录。
// loader 为 nil 即整体禁用该能力（与未接线时行为一致）；root 可为空——此时只有
// 项目上下文（InjectProjectContextResolver）能产出内容。
func (a *Agent) InjectWorkspaceContextLoader(l *agentctx.WorkspaceContextLoader, root string) {
	a.workspaceCtxLoader = l
	a.workspaceRoot = root
}

// InjectProjectContextResolver 注入按会话反查项目上下文的函数（ADR-0097）。
// nil 表示不启用项目上下文。
func (a *Agent) InjectProjectContextResolver(r func(ctx context.Context, sessionID string) *agentctx.ProjectContext) {
	a.projectResolver = r
}
