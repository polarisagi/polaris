package bootstrap

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// Bootstrapper 管理所有已注册模块的启动与优雅关停。
//
// 启动流程（Kahn 拓扑排序）：
//
//	L0（基础设施：store/security/vfs）
//	L1（认知执行：llm/memory/agent/action/sandbox）
//	L2（协同知识：swarm/learning/knowledge/channel/extension）
//	L3（接口治理：gateway/prompt/automation/eval）
//
// 关停流程（四阶 Phase）：
//
//	Phase1 停流 → Phase2 排干 → Phase3 刷盘 → Phase4 释放
type Bootstrapper struct {
	modules map[string]Bootable
	deps    *DependencyMap
	kmsKey  []byte // 内存级 KMS 密钥，Ignite 完成后立即清零
}

// NewBootstrapper 创建启动器。kmsKey 为可选主密钥（nil = 无 KMS）。
func NewBootstrapper(kmsKey []byte) *Bootstrapper {
	return &Bootstrapper{
		modules: make(map[string]Bootable),
		deps:    NewDependencyMap(),
		kmsKey:  kmsKey,
	}
}

// RegisterModule 注册一个命名模块。同名模块后注册覆盖前者。
func (b *Bootstrapper) RegisterModule(name string, mod Bootable) {
	b.modules[name] = mod
}

// RegisterDep 向依赖表注入一个外部依赖（非 Bootable 的基础设施对象）。
// 典型用途：将已打开的 *sql.DB、配置对象等注入给其他模块使用。
func (b *Bootstrapper) RegisterDep(name string, dep any) {
	b.deps.Register(name, dep)
}

// GetMasterKey 实现 ConfigProvider，供 Init 期间提取密钥（返回副本）。
func (b *Bootstrapper) GetMasterKey() []byte {
	if b.kmsKey == nil {
		return nil
	}
	cp := make([]byte, len(b.kmsKey))
	copy(cp, b.kmsKey)
	return cp
}

// Ignite 按拓扑排序依次初始化所有模块。
// 完成后立即 memclr KMS 密钥。
func (b *Bootstrapper) Ignite(ctx context.Context) error {
	// 注册自身为 ConfigProvider，供需要 KMS 的模块提取
	b.deps.Register("ConfigProvider", b)

	// 1. 拓扑排序解析依赖顺序
	order, err := b.topologicalSort()
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "bootstrap: dependency resolution failed", err)
	}

	// 2. 按拓扑顺序初始化
	for _, name := range order {
		mod := b.modules[name]
		if err := mod.Init(b.deps); err != nil {
			return apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("bootstrap: module %s init failed", name), err)
		}
		if !mod.Ready() {
			return apperr.New(apperr.CodeInternal, fmt.Sprintf("bootstrap: module %s not ready after init", name))
		}
		// 初始化成功后将自身注册到依赖表，供后续模块使用
		b.deps.Register(name, mod)
	}

	// 3. 最高红线：Init 完成后立即 memclr KMS 密钥
	if b.kmsKey != nil {
		for i := range b.kmsKey {
			b.kmsKey[i] = 0
		}
		b.kmsKey = nil
	}

	return nil
}

// ListenAndServe 阻塞等待 SIGTERM/SIGINT，触发四阶优雅关停。
func (b *Bootstrapper) ListenAndServe(ctx context.Context) {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-sigChan:
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	b.gracefulShutdown(shutdownCtx)
}

// gracefulShutdown 执行四阶关停流水线。
func (b *Bootstrapper) gracefulShutdown(ctx context.Context) {
	// Phase 1：停流——熔断外部感知，停止接收新请求
	for _, mod := range b.modules {
		if s, ok := mod.(Stage1Stopper); ok {
			_ = s.StopIngress(ctx)
		}
	}

	// Phase 2：排干——等待已接受的任务/队列处理完毕
	for _, mod := range b.modules {
		if d, ok := mod.(Stage2Drainer); ok {
			_ = d.Drain(ctx)
		}
	}

	// Phase 3：刷盘——WAL Checkpoint，确保数据落盘
	for _, mod := range b.modules {
		if f, ok := mod.(Stage3Flusher); ok {
			_ = f.Flush(ctx)
		}
	}

	// Phase 4：灭火——释放 DB 句柄、VFS 游标、子进程
	for _, mod := range b.modules {
		if c, ok := mod.(Stage4Closer); ok {
			_ = c.Close(ctx)
		}
	}
}

// topologicalSort 基于 Kahn 算法对模块进行拓扑排序（L0 → L3）。
func (b *Bootstrapper) topologicalSort() ([]string, error) {
	inDegree := make(map[string]int)
	graph := make(map[string][]string)

	for name := range b.modules {
		inDegree[name] = 0
	}

	for name, mod := range b.modules {
		for _, dep := range mod.Dependencies() {
			if _, exists := b.modules[dep]; !exists {
				return nil, apperr.New(apperr.CodeInternal, fmt.Sprintf("module %s requires missing dependency %s", name, dep))
			}
			graph[dep] = append(graph[dep], name)
			inDegree[name]++
		}
	}

	var queue []string
	for name, degree := range inDegree {
		if degree == 0 {
			queue = append(queue, name)
		}
	}

	var order []string
	for len(queue) > 0 {
		curr := queue[0]
		queue = queue[1:]
		order = append(order, curr)
		for _, neighbor := range graph[curr] {
			inDegree[neighbor]--
			if inDegree[neighbor] == 0 {
				queue = append(queue, neighbor)
			}
		}
	}

	if len(order) != len(b.modules) {
		return nil, apperr.New(apperr.CodeInternal, "circular dependency detected among registered modules")
	}
	return order, nil
}
