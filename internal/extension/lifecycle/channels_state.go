package lifecycle

import (
	"context"
	"sync"
)

const maxChannelDedupCapacity = 4096

// channelDedup 内存环形/FIFO 消息去重器，防止通道重放攻击。
type channelDedup struct {
	mu       sync.Mutex
	capacity int
	seen     map[string]struct{}
	order    []string
	head     int
}

func newChannelDedup(capacity int) *channelDedup {
	if capacity <= 0 {
		capacity = maxChannelDedupCapacity
	}
	return &channelDedup{
		capacity: capacity,
		seen:     make(map[string]struct{}),
		order:    make([]string, 0, capacity),
	}
}

// checkOrRecord 检查事件幂等键是否已处理。未见返回 true 并记录；已见返回 false。
func (d *channelDedup) checkOrRecord(pluginID, server, messageID string) bool {
	if messageID == "" {
		return true
	}
	key := pluginID + "\x00" + server + "\x00" + messageID
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.seen[key]; ok {
		return false
	}
	if len(d.order) < d.capacity {
		d.order = append(d.order, key)
	} else {
		oldest := d.order[d.head]
		delete(d.seen, oldest)
		d.order[d.head] = key
		d.head = (d.head + 1) % d.capacity
	}
	d.seen[key] = struct{}{}
	return true
}

type bindingStateEntry struct {
	generation uint64
	nextTurnID uint64
	inFlight   map[uint64]context.CancelFunc
}

// channelRuntimeState 管理通道运行期世代编号与在途轮次取消。
type channelRuntimeState struct {
	mu       sync.Mutex
	bindings map[string]*bindingStateEntry
}

func newChannelRuntimeState() *channelRuntimeState {
	return &channelRuntimeState{
		bindings: make(map[string]*bindingStateEntry),
	}
}

func bindingKey(pluginID, server string) string {
	return pluginID + "\x00" + server
}

func (s *channelRuntimeState) currentGen(pluginID, server string) uint64 {
	key := bindingKey(pluginID, server)
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.bindings[key]
	if entry == nil {
		return 0
	}
	return entry.generation
}

func (s *channelRuntimeState) cancelBinding(pluginID, server string) {
	key := bindingKey(pluginID, server)
	s.mu.Lock()
	entry := s.bindings[key]
	if entry == nil {
		entry = &bindingStateEntry{
			inFlight: make(map[uint64]context.CancelFunc),
		}
		s.bindings[key] = entry
	}
	entry.generation++
	cancels := make([]context.CancelFunc, 0, len(entry.inFlight))
	for _, cancel := range entry.inFlight {
		cancels = append(cancels, cancel)
	}
	entry.inFlight = make(map[uint64]context.CancelFunc)
	s.mu.Unlock()

	for _, cancel := range cancels {
		cancel()
	}
}

func (s *channelRuntimeState) startTurn(ctx context.Context, pluginID, server string, expectedGen uint64) (context.Context, func(), bool) {
	key := bindingKey(pluginID, server)
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.bindings[key]
	if entry == nil {
		if expectedGen != 0 {
			return ctx, nil, false
		}
		entry = &bindingStateEntry{
			inFlight: make(map[uint64]context.CancelFunc),
		}
		s.bindings[key] = entry
	}
	if entry.generation != expectedGen {
		return ctx, nil, false
	}
	entry.nextTurnID++
	turnID := entry.nextTurnID
	turnCtx, cancel := context.WithCancel(ctx)
	entry.inFlight[turnID] = cancel

	unregister := func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if current := s.bindings[key]; current != nil {
			delete(current.inFlight, turnID)
		}
	}
	return turnCtx, unregister, true
}
