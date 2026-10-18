package llm

import (
	"context"
	"strings"
)

// InjectProviderFactory 注入动态 Provider 工厂，用于按需实例化实时目录或未预注册的模型。
func (r *ProviderRegistry) InjectProviderFactory(f ProviderFactory) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.factory = f
}

// PickEntryByRef 按引用获取 providerEntry。
// 支持格式：
// 1. <provider_models 记录 ID 前缀>（与 PickProviderByRecordID 同口径）
// 2. <provider_id>/<model_id>（实时模型目录中的任意模型）
// 若 entry 存在但被熔断，返回 nil；若不存在且存在 factory，则按需实例化并写入 LRU 缓存。
func (r *ProviderRegistry) PickEntryByRef(ctx context.Context, ref string, acquire bool) *providerEntry {
	if ref == "" {
		return nil
	}

	if !strings.Contains(ref, "/") {
		return r.pickByRecordIDSuffix(ref, acquire)
	}

	if e, found := r.pickFromRegisteredOrCached(ref, acquire); found {
		return e
	}

	return r.instantiateDynamicEntry(ctx, ref, acquire)
}

func (r *ProviderRegistry) pickByRecordIDSuffix(ref string, acquire bool) *providerEntry {
	suffix := ref
	if len(suffix) >= 8 {
		suffix = suffix[:8]
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return selectBest(r.entries, func(name string, _ *providerEntry) bool {
		return strings.HasSuffix(name, "/"+suffix)
	}, acquire)
}

func (r *ProviderRegistry) pickFromRegisteredOrCached(ref string, acquire bool) (*providerEntry, bool) {
	r.mu.RLock()
	if e, ok := r.entries[ref]; ok {
		r.mu.RUnlock()
		if isEntryAvailable(e, acquire) {
			return e, true
		}
		return nil, true
	}

	if e, ok := r.dynamicEntries[ref]; ok {
		r.mu.RUnlock()
		if isEntryAvailable(e, acquire) {
			r.touchDynamicLRU(ref)
			return e, true
		}
		return nil, true
	}
	r.mu.RUnlock()

	return nil, false
}

func (r *ProviderRegistry) instantiateDynamicEntry(ctx context.Context, ref string, acquire bool) *providerEntry {
	r.mu.RLock()
	factory := r.factory
	r.mu.RUnlock()
	if factory == nil {
		return nil
	}

	parts := strings.SplitN(ref, "/", 2)
	p, displayName, err := factory.CreateProvider(ctx, parts[0], parts[1])
	if err != nil || p == nil {
		return nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// 双重检查
	if e, ok := r.dynamicEntries[ref]; ok {
		if isEntryAvailable(e, acquire) {
			return e
		}
		return nil
	}

	entry := r.newEntry(ref, displayName, "", p)
	r.addDynamicEntryLocked(ref, entry)

	if isEntryAvailable(entry, acquire) {
		return entry
	}
	return nil
}

func isEntryAvailable(e *providerEntry, acquire bool) bool {
	if e == nil || !e.cb.Available() || !e.winBreaker.Allow() {
		return false
	}
	return !acquire || e.cb.Allow()
}

// touchDynamicLRU 将 ref 移到 LRU 队列末尾（最近使用）。
func (r *ProviderRegistry) touchDynamicLRU(ref string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, k := range r.dynamicKeys {
		if k == ref {
			r.dynamicKeys = append(r.dynamicKeys[:i], r.dynamicKeys[i+1:]...)
			r.dynamicKeys = append(r.dynamicKeys, ref)
			return
		}
	}
}

// addDynamicEntryLocked 将动态 entry 写入缓存，超出容量时淘汰最旧条目（调用方持写锁）。
func (r *ProviderRegistry) addDynamicEntryLocked(ref string, entry *providerEntry) {
	r.dynamicEntries[ref] = entry
	r.dynamicKeys = append(r.dynamicKeys, ref)

	maxDynamic := r.dynamicMax
	if maxDynamic <= 0 {
		maxDynamic = 32
	}
	for len(r.dynamicKeys) > maxDynamic {
		oldest := r.dynamicKeys[0]
		r.dynamicKeys = r.dynamicKeys[1:]
		delete(r.dynamicEntries, oldest)
	}
}
