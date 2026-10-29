package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agentctx "github.com/polarisagi/polaris/internal/agent/context"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

func canonicalDir(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// 项目绑定目录 + 信任 → AGENTS.md 与项目指令都进可信通道，且 resolver 收到的是本会话 ID。
func TestRefreshWorkspaceContext_ProjectTrustedRootAndInstructions(t *testing.T) {
	root := canonicalDir(t)
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("项目规则R"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := NewAgent("sess-p", nil, nil)
	a.InjectWorkspaceContextLoader(agentctx.NewWorkspaceContextLoader(nil), "")
	var gotSession string
	a.InjectProjectContextResolver(func(_ context.Context, sessionID string) *agentctx.ProjectContext {
		gotSession = sessionID
		return &agentctx.ProjectContext{Root: root, Trusted: true, Instructions: "用中文回答"}
	})

	a.refreshWorkspaceContext(context.Background())

	if gotSession != "sess-p" {
		t.Fatalf("resolver 应以本会话 ID 调用，got %q", gotSession)
	}
	tr := a.sCtx.WorkspaceContextTrusted
	if !strings.Contains(tr, "项目规则R") || !strings.Contains(tr, "用中文回答") {
		t.Fatalf("信任项目的目录文件与指令都应进可信通道: %q", tr)
	}
	if a.sCtx.WorkspaceContextUntrusted != "" {
		t.Fatalf("不应有低信任内容: %q", a.sCtx.WorkspaceContextUntrusted)
	}
}

// 项目未信任 → 目录文件只能进围栏区；用户自撰指令仍可信。
func TestRefreshWorkspaceContext_UntrustedProjectFencesFiles(t *testing.T) {
	root := canonicalDir(t)
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("来自仓库的内容"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := NewAgent("sess-u", nil, nil)
	a.InjectWorkspaceContextLoader(agentctx.NewWorkspaceContextLoader(nil), "")
	a.InjectProjectContextResolver(func(context.Context, string) *agentctx.ProjectContext {
		return &agentctx.ProjectContext{Root: root, Trusted: false, Instructions: "我的指令"}
	})

	a.refreshWorkspaceContext(context.Background())

	if strings.Contains(a.sCtx.WorkspaceContextTrusted, "来自仓库的内容") {
		t.Fatalf("未信任项目的仓库文件不得进可信通道: %q", a.sCtx.WorkspaceContextTrusted)
	}
	if !strings.Contains(a.sCtx.WorkspaceContextTrusted, "我的指令") {
		t.Fatalf("用户自撰项目指令应进可信通道: %q", a.sCtx.WorkspaceContextTrusted)
	}
	if !strings.Contains(a.sCtx.WorkspaceContextUntrusted, "来自仓库的内容") {
		t.Fatalf("未信任目录文件应进围栏区: %q", a.sCtx.WorkspaceContextUntrusted)
	}
}

// 无目录项目（Root 为空）：不回退读别的目录，只带指令。
func TestRefreshWorkspaceContext_NoDirProjectInstructionsOnly(t *testing.T) {
	a := NewAgent("sess-n", nil, nil)
	a.InjectWorkspaceContextLoader(agentctx.NewWorkspaceContextLoader(nil), "")
	a.InjectProjectContextResolver(func(context.Context, string) *agentctx.ProjectContext {
		return &agentctx.ProjectContext{Instructions: "只有指令"}
	})
	a.refreshWorkspaceContext(context.Background())
	if !strings.Contains(a.sCtx.WorkspaceContextTrusted, "只有指令") {
		t.Fatalf("无目录项目应仍带指令: %q", a.sCtx.WorkspaceContextTrusted)
	}
}

// resolver 返回 nil（默认项目 / 会话尚未落库 / 查询失败）→ 保持既有行为：
// 无 workspaceRoot 时什么都不装载，且不 panic。
func TestRefreshWorkspaceContext_NilProjectKeepsLegacyBehavior(t *testing.T) {
	a := NewAgent("sess-l", nil, nil)
	a.InjectWorkspaceContextLoader(agentctx.NewWorkspaceContextLoader(nil), "")
	a.InjectProjectContextResolver(func(context.Context, string) *agentctx.ProjectContext { return nil })
	a.refreshWorkspaceContext(context.Background())
	if a.sCtx.WorkspaceContextTrusted != "" || a.sCtx.WorkspaceContextUntrusted != "" {
		t.Fatalf("无项目上下文时应为空")
	}
}

// 指令改动下一轮生效（不是装配时快照）。
func TestRefreshWorkspaceContext_ReResolvesEveryRefresh(t *testing.T) {
	a := NewAgent("sess-r", nil, nil)
	a.InjectWorkspaceContextLoader(agentctx.NewWorkspaceContextLoader(nil), "")
	cur := "第一版"
	a.InjectProjectContextResolver(func(context.Context, string) *agentctx.ProjectContext {
		return &agentctx.ProjectContext{Instructions: cur}
	})
	a.refreshWorkspaceContext(context.Background())
	cur = "第二版"
	a.refreshWorkspaceContext(context.Background())
	if !strings.Contains(a.sCtx.WorkspaceContextTrusted, "第二版") || strings.Contains(a.sCtx.WorkspaceContextTrusted, "第一版") {
		t.Fatalf("应每轮重新解析: %q", a.sCtx.WorkspaceContextTrusted)
	}
}

// 项目工作目录随任务域 ctx 注入工具调用（ADR-0097 决策五）；不依赖工作区上下文装载器。
func TestProjectRoot_FlowsIntoTaskScopeCtx(t *testing.T) {
	root := canonicalDir(t)
	a := NewAgent("sess-root", nil, nil) // 刻意不注入 WorkspaceContextLoader
	a.InjectProjectContextResolver(func(context.Context, string) *agentctx.ProjectContext {
		return &agentctx.ProjectContext{Root: root}
	})
	a.refreshWorkspaceContext(context.Background())

	if got := protocol.ProjectRootFrom(a.withTaskScopeCtx(context.Background())); got != root {
		t.Fatalf("工具 ctx 应携带项目目录 %q，got %q", root, got)
	}
}

// 存库的目录后来被替换成软链 → 不再作为访问根（与信任撤销同一判据）；
// 会话移出项目 → 下一轮清空。
func TestProjectRoot_RevokedOnSymlinkAndClearedWhenUnbound(t *testing.T) {
	base := canonicalDir(t)
	target := filepath.Join(base, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink 不可用: %v", err)
	}
	pc := &agentctx.ProjectContext{Root: link}
	a := NewAgent("sess-link", nil, nil)
	a.InjectProjectContextResolver(func(context.Context, string) *agentctx.ProjectContext { return pc })

	a.refreshWorkspaceContext(context.Background())
	if got := protocol.ProjectRootFrom(a.withTaskScopeCtx(context.Background())); got != "" {
		t.Fatalf("软链根不得成为访问根，got %q", got)
	}

	pc = &agentctx.ProjectContext{Root: target}
	a.refreshWorkspaceContext(context.Background())
	if got := protocol.ProjectRootFrom(a.withTaskScopeCtx(context.Background())); got != target {
		t.Fatalf("got %q", got)
	}
	pc = nil
	a.refreshWorkspaceContext(context.Background())
	if got := protocol.ProjectRootFrom(a.withTaskScopeCtx(context.Background())); got != "" {
		t.Fatalf("会话不再属于有目录的项目后应清空，got %q", got)
	}
}

// 委派子 Agent 的会话 ID 是一次性的，本会话解析不到项目时按共享记忆命名空间（发起方
// 根会话 ID）继承项目（ADR-0097 决策三补）。
func TestProjectScope_SubAgentInheritsViaNamespace(t *testing.T) {
	root := canonicalDir(t)
	a := NewAgent("headless-123", nil, nil)
	var asked []string
	a.InjectProjectContextResolver(func(_ context.Context, sessionID string) *agentctx.ProjectContext {
		asked = append(asked, sessionID)
		if sessionID == "sess-parent" {
			return &agentctx.ProjectContext{ID: "prj_x", Root: root, Instructions: "项目指令X"}
		}
		return nil
	})
	a.SetMemoryNamespace("sess-parent")
	a.refreshWorkspaceContext(context.Background())

	if len(asked) != 2 || asked[0] != "headless-123" || asked[1] != "sess-parent" {
		t.Fatalf("解析顺序应为 本会话 → 命名空间，got %v", asked)
	}
	ctx := a.withTaskScopeCtx(context.Background())
	if got := protocol.ProjectIDFrom(ctx); got != "prj_x" {
		t.Fatalf("子 Agent 应继承项目 ID，got %q", got)
	}
	if got := protocol.ProjectRootFrom(ctx); got != root {
		t.Fatalf("子 Agent 应继承项目工作目录，got %q", got)
	}
	if got := a.toProtocolCtx().ProjectID; got != "prj_x" {
		t.Fatalf("FSM 上下文应带项目 ID，got %q", got)
	}
}

// 解析不到项目的 Agent 以默认项目为界；情景写入显式打上当前项目。
func TestProjectScope_EpisodicWriteTaggedAndDefaultFallback(t *testing.T) {
	ep := &mockEpisodicMemForIntegration{}
	mem := &mockMemoryForIntegration{episodic: ep, working: &mockWorkingMemForIntegration{immutable: &mockImmutableCoreForIntegration{}}}

	a := NewAgent("agent-0", nil, nil)
	a.InjectMemory(mem)
	a.InjectProjectContextResolver(func(context.Context, string) *agentctx.ProjectContext { return nil })
	a.refreshWorkspaceContext(context.Background())
	if got := a.currentProjectID(); got != types.DefaultProjectID {
		t.Fatalf("无项目应按默认项目，got %q", got)
	}

	a.InjectProjectContextResolver(func(context.Context, string) *agentctx.ProjectContext {
		return &agentctx.ProjectContext{ID: "prj_w"}
	})
	a.refreshWorkspaceContext(context.Background())
	a.writeEpisodicWithExtract(context.Background(), types.Event{ID: "e1", Type: "x", Payload: []byte("p")})
	if len(ep.events) != 1 || ep.events[0].ProjectID != "prj_w" {
		t.Fatalf("情景写入应打上当前项目，got %+v", ep.events)
	}
}
