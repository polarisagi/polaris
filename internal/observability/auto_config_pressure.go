package observability

import (
	"context"
	"log/slog"
	"runtime"
	"time"

	"github.com/polarisagi/polaris/internal/observability/probe"
	"github.com/polarisagi/polaris/internal/protocol"
)

// ============================================================================
// 内存压力回调与降级/恢复路径（R7 拆分自 auto_config.go）。
// 结构体/构造/静态配置计算见 auto_config.go；Tier 数值参数表见 auto_config_tiers.go。
// ============================================================================

// MemoryPressureCallback is called by probe.OSMemoryGuard when pressure level changes.
// Hysteresis: 256 MB threshold before reassessing to avoid thrashing.
func (ac *AutoConfig) MemoryPressureCallback(availableMB uint64, level probe.DegradationLevel) {
	prevMB := ac.Gate.GetAvailableMemoryMB()
	if probe.AbsDiff(prevMB, availableMB) < 256 {
		return
	}
	ac.Gate.Reassess(availableMB)

	switch level {
	case probe.DegradationCritical:
		// 顺序（M01 §8.2）：先 Disable Gate 阻止新的本地推理请求进入 → 再
		// UnloadModel 真正释放已加载模型的常驻内存 → 再 GC 把内存归还给 OS
		// → 最后清理沙箱。UnloadModel 卸载的是"当前已加载模型"，不会强杀
		// 正在处理中的单次推理调用（LocalProvider.UnloadModel 实现自身负责
		// 等待/中断在途请求，不是本回调的职责）。
		ac.Gate.Override(probe.FeatureQLoRA, probe.FeatureDisabled)
		ac.Gate.Override(probe.FeatureLargeLocalLLM, probe.FeatureDisabled)
		ac.Gate.Override(probe.FeatureLocalInference, probe.FeatureDisabled)
		ac.unloadLocalModel()
		// 触发 Go GC 尽快归还内存给 OS，降低 OOM 风险
		runtime.GC()
		// 通知 SurrealDB FFI 侧执行内存压缩（best-effort，忽略返回值）
		if ac.surrealPurger != nil {
			ac.surrealPurger()
		}
		// M07 §12 L3：kill 全部非关键沙箱
		if ac.Sandbox != nil {
			ac.Sandbox.KillAllNonCritical(context.Background())
		}
	case probe.DegradationWarning:
		ac.Gate.Override(probe.FeatureQLoRA, probe.FeatureDisabled)
		ac.Gate.Override(probe.FeatureLargeLocalLLM, probe.FeatureDegraded)
		// M07 §12 L2：kill 空闲沙箱
		if ac.Sandbox != nil {
			ac.Sandbox.KillIdleSandboxes(context.Background())
		}
	case probe.DegradationCaution:
		ac.Gate.Override(probe.FeatureQLoRA, probe.FeatureDegraded)
		// M07 §12 L1：禁止启动新 Wasm 实例
		if ac.Sandbox != nil {
			ac.Sandbox.DisableNewInstances(true)
		}
	case probe.DegradationNone:
		ac.clearOverrides()
		if ac.Sandbox != nil {
			ac.Sandbox.DisableNewInstances(false)
		}
	}
}

// unloadLocalModel best-effort 卸载 "llama-local"（M01 canonical Provider 名）。
// unloader 未注入、Provider 未注册、类型断言失败、UnloadModel 报错，均只记录日志
// 不中断降级流程——内存降级路径本身要求 fail-open（宁可少释放一点内存也不能因为
// 卸载失败而阻塞 GC/沙箱清理这些同样重要的降级步骤）。
// 内部固定 3s 超时（MemoryPressureCallback 本身不接收 ctx，运行在 5s 轮询
// goroutine 上，不能无限阻塞降级路径）。
func (ac *AutoConfig) unloadLocalModel() {
	if ac.localUnloader == nil {
		return
	}
	provider, ok := ac.localUnloader.Get("llama-local")
	if !ok {
		return
	}
	localProvider, ok := provider.(protocol.LocalProvider)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := localProvider.UnloadModel(ctx); err != nil {
		slog.Error("observability: failed to unload local model under critical memory pressure", "err", err)
		return
	}
	slog.Info("observability: local model unloaded due to critical memory pressure")
}

// RunMemoryWatcher polls available system RAM every 5s and drives MemoryPressureCallback.
// Call as a goroutine after AutoConfig is initialized; safe when ac is nil (no-op).
func (ac *AutoConfig) RunMemoryWatcher(ctx context.Context) {
	if ac == nil {
		return
	}
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			availMB := probe.ProbeAvailableMemoryMB()
			level := ac.Guard.CheckAndProtect(availMB)
			ac.MemoryPressureCallback(availMB, level)
		}
	}
}

func (ac *AutoConfig) clearOverrides() {
	for _, f := range []probe.Feature{
		probe.FeatureQLoRA, probe.FeatureLargeLocalLLM, probe.FeatureLocalInference,
		probe.FeaturePRMTraining, probe.FeatureL3Sandbox, probe.FeatureLogicCollapse,
		probe.FeatureActivationSteer, probe.FeatureGraphRAGFull,
	} {
		ac.Gate.Override(f, probe.FeatureState(-1))
	}
}
