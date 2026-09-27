package mcp

import (
	"bytes"
	"context"
	"log/slog"
	"sync"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// refreshState 单个 server 的刷新合并态：running 表示当前是否已有一次 refreshTools
// 在执行 ListTools；pending 表示运行期间又到达了新的刷新请求。短时间内连续多条
// notifications/tools/list_changed（新纪元订阅通知或旧纪元自发通知）合并为运行结束后
// 至多再补一轮，而不是无界并发/排队 ListTools（HE-5：这里退化为最小的"忙碌+补一次"
// 标记，不是完整状态机）。
type refreshState struct {
	mu      sync.Mutex
	running bool
	pending bool
}

// refreshStateFor 取（或创建）指定 serverID 的合并态。用 sync.Map 而非 map+锁：
// MCPManager 已有的 m.mu 保护的是 entries，refreshState 的生命周期与 entries 无关
// （server 被 Remove 后再 Add 回来，合并态可以重新开始），没必要绑同一把锁。
func (m *MCPManager) refreshStateFor(serverID string) *refreshState {
	v, _ := m.refreshing.LoadOrStore(serverID, &refreshState{})
	return v.(*refreshState) //nolint:forcetypeassert // LoadOrStore 只会存 *refreshState
}

// refreshTools 对指定 server 重新拉取工具列表，注销已消失的工具、注册新增/变更的工具，
// 更新 entry.tools 并触发 onToolsChanged。并发安全：同一 server 的刷新串行化——运行期间
// 到达的后续调用立即返回 nil（不做 IO），合并为本轮结束后至多一次追加执行。IO
// （ListTools/注册/注销）全部在 m.mu 之外执行，仅在读写 entry 快照时短暂持锁（与 Add()
// 段2a 同一约束：长时 IO 不得在 m.mu 持锁期间进行）。失败记 Warn（HE-1），不向上抛给
// 异步调用方（通知处理器/订阅协程都不关心返回值，故它们直接调用不接返回值）。
func (m *MCPManager) refreshTools(ctx context.Context, serverID string) error {
	st := m.refreshStateFor(serverID)

	st.mu.Lock()
	if st.running {
		st.pending = true
		st.mu.Unlock()
		return nil
	}
	st.running = true
	st.mu.Unlock()

	var lastErr error
	for {
		lastErr = m.doRefreshTools(ctx, serverID)
		if lastErr != nil {
			slog.Warn("mcp: refreshTools failed", "server", serverID, "err", lastErr)
		}
		st.mu.Lock()
		if !st.pending {
			st.running = false
			st.mu.Unlock()
			return lastErr
		}
		st.pending = false
		st.mu.Unlock()
	}
}

// doRefreshTools 执行一轮实际刷新：ListTools → 注销消失的工具 → 注册新增/变更的工具 →
// 写回 entry.tools。
func (m *MCPManager) doRefreshTools(ctx context.Context, serverID string) error {
	client, name, oldTools, ok := m.refreshSnapshot(serverID)
	if !ok {
		return apperr.New(apperr.CodeNotFound, "mcp: refreshTools: server not connected: "+serverID)
	}

	listCtx, cancel := context.WithTimeout(ctx, client.cfg.Timeout)
	defer cancel()
	newTools, err := client.ListTools(listCtx)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "mcp: refreshTools: list tools "+serverID, err)
	}

	toRegister, toUnregister, changed := diffTools(oldTools, newTools)
	if len(toUnregister) > 0 {
		m.unregisterTools(name, toUnregister)
	}
	var validNew []MCPTool
	if len(toRegister) > 0 {
		validNew = m.registerTools(serverID, name, client, toRegister)
	}

	added := m.commitRefreshedTools(serverID, client, oldTools, changed, validNew)
	slog.Info("mcp: tools refreshed", "server", serverID, "added", added, "removed", len(toUnregister))
	return nil
}

// refreshSnapshot 短暂持读锁，取指定 server 当前 client/name/tools 快照，供
// doRefreshTools 在锁外执行 ListTools 之前有一个一致的起点。
func (m *MCPManager) refreshSnapshot(serverID string) (client *MCPClient, name string, tools []MCPTool, ok bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	e, exists := m.entries[serverID]
	if !exists || e.client == nil {
		return nil, "", nil, false
	}
	return e.client, e.name, e.tools, true
}

// commitRefreshedTools 把 registerTools 实际注册成功的新/变更工具与未变化的旧工具合并，
// 写回 entry.tools 并触发 onToolsChanged；若 entry 在 ListTools 执行期间已被 Add()/
// Remove() 替换（client 指针不再相同），放弃本次写回，避免践踏更新后的状态。回调必须在
// 锁外触发，理由与 Add() 段3 相同（回调内反向加锁会死锁）。返回值供调用方记录观测日志。
func (m *MCPManager) commitRefreshedTools(serverID string, client *MCPClient, oldTools []MCPTool,
	changed map[string]bool, validNew []MCPTool) int {
	m.mu.Lock()
	e, ok := m.entries[serverID]
	if !ok || e.client != client {
		m.mu.Unlock()
		return 0
	}
	unchanged := make([]MCPTool, 0, len(oldTools))
	for _, t := range oldTools {
		if !changed[t.Name] {
			unchanged = append(unchanged, t)
		}
	}
	e.tools = append(unchanged, validNew...)
	notify := m.onToolsChanged
	m.mu.Unlock()
	if notify != nil {
		notify()
	}
	return len(validNew)
}

// diffTools 比较刷新前后的工具集合（按 Name 做 key）：toRegister 为新增或定义变化的
// 工具（RegisterRich/Register 对同名 key 直接覆盖，见 internal/sandbox 的注册表实现，
// 不需要先注销旧定义）；toUnregister 为消失的工具；changed 记录所有名称有变化的
// key（新增/变化/消失），供 commitRefreshedTools 从旧工具列表里筛掉需要替换的项。
func diffTools(old, latest []MCPTool) (toRegister, toUnregister []MCPTool, changed map[string]bool) {
	oldByName := make(map[string]MCPTool, len(old))
	for _, t := range old {
		oldByName[t.Name] = t
	}
	changed = make(map[string]bool)
	newByName := make(map[string]bool, len(latest))
	for _, t := range latest {
		newByName[t.Name] = true
		if o, existed := oldByName[t.Name]; !existed || !toolEqual(o, t) {
			toRegister = append(toRegister, t)
			changed[t.Name] = true
		}
	}
	for _, t := range old {
		if !newByName[t.Name] {
			toUnregister = append(toUnregister, t)
			changed[t.Name] = true
		}
	}
	return toRegister, toUnregister, changed
}

// toolEqual 判断两个同名工具的定义是否一致（描述 + inputSchema 字节相等）。
func toolEqual(a, b MCPTool) bool {
	return a.Description == b.Description && bytes.Equal(a.InputSchema, b.InputSchema)
}
