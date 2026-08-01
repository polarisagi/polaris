package sandbox

import (
	"context"
	"runtime"
	"testing"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// ─── 阶段03 R-05：沙箱可信来源 InProcess 降级 fail-closed 回归测试 ─────────
//
// 问题背景：routeWasm 此前对可信来源（mustIsolate==false）在 Wasm/Container/
// Remote 均不可用时直接静默降级 InProcess，只打一条 Warn。"可信"是安全维度
// 的判断，不等于"稳定"——可信来源的代码仍可能死循环/OOM/panic，InProcess
// 执行会直接拖垮宿主进程。修复后默认 fail-closed，需 SandboxConfig.
// AllowTrustedInProcessFallback 显式 opt-in 才允许降级。
//
// RecordSandboxDowngrade 的 OTel counter 值本身不在此处断言：本包所有指标
// instrument 均由 InitMetrics 通过 sync.Once 全局单例注册（Tier-0 legacy
// 路径下测试环境中恒为 nil，RecordXxx 系列函数按设计静默 no-op），仓内其它
// 阶段（如 R-02 PIIDesensitizer 淘汰计数）新增的 OTel counter 同样未见对
// 数值本身做单测断言，属于既有测试深度的一致做法；此处覆盖的是行为契约
// （降级是否发生、是否报错），counter 调用点本身随生产代码路径被覆盖。

// newTrustFallbackRouter 构造一个 Wasm/Container/Remote 均未注入的 SandboxRouter
// （模拟"未编译 Wasm 引擎、无容器运行时"的开发环境），用于驱动 routeWasm 的
// 降级判定分支。
func newTrustFallbackRouter() *SandboxRouter {
	inProc := NewInProcessSandbox(config.DefaultThresholds().M7Tool)
	inProc.Register("trusted-tool", func(_ context.Context, _ []byte) ([]byte, error) {
		return []byte(`{}`), nil
	})
	return NewSandboxRouter(inProc, nil, nil, runtime.GOOS, 0)
}

// TestSandboxRouter_TrustedInProcessFallback_DefaultFailClosed 验证默认配置
// （AllowTrustedInProcessFallback 零值 false）下，可信来源请求 L2/Wasm 但
// Wasm/Container/Remote 均不可用时，拒绝静默降级，返回明确错误而非成功。
func TestSandboxRouter_TrustedInProcessFallback_DefaultFailClosed(t *testing.T) {
	router := newTrustFallbackRouter()

	_, err := router.RouteByTier(types.SandboxWasm, types.TrustOfficial)
	if err == nil {
		t.Fatal("期望 fail-closed 报错，实际返回 nil error（疑似静默降级未被拦截）")
	}
	if !apperr.IsCode(err, apperr.CodeSandboxTier0Limit) {
		t.Errorf("期望 CodeSandboxTier0Limit，实际: %v", err)
	}
}

// TestSandboxRouter_TrustedInProcessFallback_OptInDowngrades 验证显式
// WithAllowTrustedInProcessFallback(true) 后，可信来源确实能降级到 InProcess
// 并正常执行（与旧行为等价，仅需显式 opt-in）。
func TestSandboxRouter_TrustedInProcessFallback_OptInDowngrades(t *testing.T) {
	router := newTrustFallbackRouter()
	router.WithAllowTrustedInProcessFallback(true)

	provider, err := router.RouteByTier(types.SandboxWasm, types.TrustOfficial)
	if err != nil {
		t.Fatalf("opt-in 后不应报错: %v", err)
	}
	if provider != router.inProcess {
		t.Error("期望降级路由至 inProcess provider")
	}

	res, err := router.Execute(context.Background(), types.Tool{
		Source: types.ToolLLMGenerated, Capability: types.CapReadOnly,
		Name: "trusted-tool", TrustTier: types.TrustOfficial,
	}, nil, types.TaintNone)
	if err != nil {
		t.Fatalf("Execute 不应报错: %v", err)
	}
	if !res.Success {
		t.Fatalf("期望执行成功: %s", res.Error)
	}
}

// TestSandboxRouter_UntrustedInProcessFallback_AlwaysForbidden 验证不可信来源
// （mustIsolate==true）无论 AllowTrustedInProcessFallback 如何配置，Wasm 不可用
// 时恒 CodeForbidden——该开关只对"可信来源"生效，不可信来源没有降级余地。
func TestSandboxRouter_UntrustedInProcessFallback_AlwaysForbidden(t *testing.T) {
	for _, allow := range []bool{false, true} {
		router := newTrustFallbackRouter()
		router.WithAllowTrustedInProcessFallback(allow)

		_, err := router.RouteByTier(types.SandboxWasm, types.TrustUntrusted)
		if err == nil {
			t.Fatalf("allow=%v: 期望不可信来源恒报错，实际返回 nil", allow)
		}
		if !apperr.IsCode(err, apperr.CodeForbidden) {
			t.Errorf("allow=%v: 期望 CodeForbidden，实际: %v", allow, err)
		}
	}
}
