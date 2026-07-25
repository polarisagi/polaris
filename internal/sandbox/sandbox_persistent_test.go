package sandbox

import (
	"context"
	"runtime"
	"testing"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// TestPersistentSandbox_AvailableIsAlwaysFalse 锁定 D4/ADR-0078 的核心不变量：
// 在没有真实 checkpoint/restore 后端接入之前，Available() 必须恒定返回
// false——这条断言本身就是"诚实占位"承诺的可验证边界（HE-2）。一旦未来有人
// 接入真实后端并让这个测试失败，说明设计已经变化，需要同步更新本测试与
// ADR-0078，而不是让假阳性可用性静默混进生产。
func TestPersistentSandbox_AvailableIsAlwaysFalse(t *testing.T) {
	p := NewPersistentSandbox("criu")
	if p.Available() {
		t.Fatal("PersistentSandbox.Available() must remain false until a real checkpoint/restore backend is implemented")
	}
	if p.Backend() != "criu" {
		t.Fatalf("expected backend to echo constructor arg, got %q", p.Backend())
	}
}

func TestPersistentSandbox_BackendDefaultsWhenEmpty(t *testing.T) {
	p := NewPersistentSandbox("")
	if p.Backend() != "unimplemented" {
		t.Fatalf("expected default backend label 'unimplemented', got %q", p.Backend())
	}
}

// TestPersistentSandbox_RunReturnsUnimplemented 验证纵深防御：即便调用方绕过
// Available() 检查直接调用 Run()，也不会得到静默的假成功。
func TestPersistentSandbox_RunReturnsUnimplemented(t *testing.T) {
	p := NewPersistentSandbox("criu")
	_, err := p.Run(context.Background(), SandboxSpec{ToolName: "some-tool"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !apperr.IsCode(err, apperr.CodeUnimplemented) {
		t.Fatalf("expected CodeUnimplemented, got %v", err)
	}
}

// TestSandboxRouter_PersistentTier_FallsBackToContainer 验证 RouteByTier 在
// persistent 注入但 Available()==false 时，按设计文档"否则保持现状"的降级
// 语义回退到 Container（与既有 SandboxContainer 分支一致），不会误路由到
// 未实现的 L4 后端。
func TestSandboxRouter_PersistentTier_FallsBackToContainer(t *testing.T) {
	inProc := NewInProcessSandbox(config.DefaultThresholds().M7Tool)
	container := NewContainerSandbox("bwrap", runtime.GOOS, 2, nil, config.DefaultThresholds().M7Tool)
	router := NewSandboxRouter(inProc, container, nil, runtime.GOOS, 2)
	router.WithPersistent(NewPersistentSandbox("criu"))

	provider, err := router.RouteByTier(types.SandboxPersistent, types.TrustSystem)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if provider != SandboxProvider(container) {
		t.Fatalf("expected fallback to container provider, got %T", provider)
	}
}

// TestSandboxRouter_PersistentTier_NoFallbackFailsClosed 验证既无可用
// persistent 后端、也无 Container/Remote 兜底时，fail-closed 拒绝而非静默
// 降级到更弱的隔离级别。
func TestSandboxRouter_PersistentTier_NoFallbackFailsClosed(t *testing.T) {
	inProc := NewInProcessSandbox(config.DefaultThresholds().M7Tool)
	router := NewSandboxRouter(inProc, nil, nil, runtime.GOOS, 2)
	router.WithPersistent(NewPersistentSandbox("criu"))

	_, err := router.RouteByTier(types.SandboxPersistent, types.TrustSystem)
	if err == nil {
		t.Fatal("expected fail-closed error when no persistent/container/remote backend is available")
	}
	if !apperr.IsCode(err, apperr.CodeForbidden) {
		t.Fatalf("expected CodeForbidden, got %v", err)
	}
}

// TestSandboxRouter_PersistentTier_WithoutInjection 验证从未调用
// WithPersistent 时（r.persistent 保持 nil，符合 Tier-0/1 默认不装配的
// 现状）仍能正确降级，不会因 nil 指针方法调用而 panic。
func TestSandboxRouter_PersistentTier_WithoutInjection(t *testing.T) {
	inProc := NewInProcessSandbox(config.DefaultThresholds().M7Tool)
	container := NewContainerSandbox("bwrap", runtime.GOOS, 2, nil, config.DefaultThresholds().M7Tool)
	router := NewSandboxRouter(inProc, container, nil, runtime.GOOS, 2)

	provider, err := router.RouteByTier(types.SandboxPersistent, types.TrustSystem)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if provider != SandboxProvider(container) {
		t.Fatalf("expected fallback to container provider, got %T", provider)
	}
}
