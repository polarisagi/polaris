package agentctx

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// canonicalTempDir 返回规范路径（EvalSymlinks）的临时目录：macOS 的 t.TempDir()
// 位于 /var → /private/var 软链之下，生产里存库的 root_path 也是规范路径。
func canonicalTempDir(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	return d
}

func writeProjFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func findDoc(docs []WorkspaceContext, rel string) *WorkspaceContext {
	for i := range docs {
		if docs[i].RelPath == rel {
			return &docs[i]
		}
	}
	return nil
}

func TestListForProject_TrustFollowsProjectFlag(t *testing.T) {
	root := canonicalTempDir(t)
	writeProjFile(t, filepath.Join(root, "AGENTS.md"), "规则A")
	l := NewWorkspaceContextLoader(nil)

	untrusted := findDoc(l.ListForProject(context.Background(), ProjectContext{Root: root}), "AGENTS.md")
	if untrusted == nil || untrusted.Trusted {
		t.Fatalf("项目未信任时上下文文件必须走围栏区: %+v", untrusted)
	}
	trusted := findDoc(l.ListForProject(context.Background(), ProjectContext{Root: root, Trusted: true}), "AGENTS.md")
	if trusted == nil || !trusted.Trusted {
		t.Fatalf("项目显式信任后应为可信: %+v", trusted)
	}
}

func TestListForProject_InstructionsAreTrustedEvenWithoutRoot(t *testing.T) {
	l := NewWorkspaceContextLoader(nil)
	docs := l.ListForProject(context.Background(), ProjectContext{Instructions: "  先跑测试  "})
	d := findDoc(docs, projectInstructionsRelPath)
	if d == nil || !d.Trusted || d.Content != "先跑测试" {
		t.Fatalf("无目录项目的指令应作为可信文档装载: %+v", docs)
	}
	if got := RenderTrusted(docs); got == "" {
		t.Fatalf("可信指令应进入 RenderTrusted")
	}
}

// 根目录存库后被换成软链：EvalSymlinks 结果 ≠ 存库路径 → 项目信任必须失效。
func TestListForProject_TrustRevokedWhenRootBecomesSymlink(t *testing.T) {
	base := canonicalTempDir(t)
	real := filepath.Join(base, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	writeProjFile(t, filepath.Join(real, "AGENTS.md"), "来自别处")
	link := filepath.Join(base, "proj")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink 不可用: %v", err)
	}

	l := NewWorkspaceContextLoader(nil)
	d := findDoc(l.ListForProject(context.Background(), ProjectContext{Root: link, Trusted: true}), "AGENTS.md")
	if d == nil {
		t.Fatalf("软链根目录内的文件仍应可读（只是不再可信）")
	}
	if d.Trusted {
		t.Fatalf("存库路径已变成软链，项目信任必须失效")
	}
}

// AGENTS.md 是指向根目录之外的软链（如 ~/.ssh/id_rsa）：必须跳过，而非读进 Prompt。
func TestLoad_SkipsFileSymlinkEscapingRoot(t *testing.T) {
	base := canonicalTempDir(t)
	secret := filepath.Join(base, "secret.txt")
	writeProjFile(t, secret, "TOP-SECRET")
	root := filepath.Join(base, "proj")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(root, "AGENTS.md")); err != nil {
		t.Skipf("symlink 不可用: %v", err)
	}

	l := NewWorkspaceContextLoader([]string{root}) // 即便在全局信任列表内也不得读出
	if docs := l.Load(context.Background(), root); len(docs) != 0 {
		t.Fatalf("逃逸根目录的软链文件必须被跳过，got %+v", docs)
	}
	if docs := l.ListForProject(context.Background(), ProjectContext{Root: root, Trusted: true}); len(docs) != 0 {
		t.Fatalf("ListForProject 同样必须跳过，got %+v", docs)
	}
}
