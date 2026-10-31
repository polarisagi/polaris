package memory

import (
	"reflect"

	"github.com/polarisagi/polaris/internal/memory/store"
	"github.com/polarisagi/polaris/internal/protocol"
)

// SessionMemoryFacade 避免各 Agent 的请求期提示词字段落到同一 ImmutableCore。
type SessionMemoryFacade struct {
	protocol.MemoryFacade
	core protocol.ImmutableCore
}

var _ protocol.MemoryFacade = (*SessionMemoryFacade)(nil)

// NewSessionMemoryFacade 保留长期记忆共享，避免跨 Agent 污染提示词字段。
func NewSessionMemoryFacade(base protocol.MemoryFacade) *SessionMemoryFacade {
	if isNilInterface(base) {
		return nil
	}

	view := &SessionMemoryFacade{MemoryFacade: base}
	baseCore := base.ImmutableCore()
	if isNilInterface(baseCore) || baseCore.Fields() == nil {
		return view
	}

	fields := *baseCore.Fields()
	fields.UserPreferences = cloneUserPreferences(fields.UserPreferences)
	// 这三项由请求或用户画像装载，每个 Agent 必须从干净视图开始。
	fields.AmbientContext = ""
	fields.VolatileBlock = ""
	fields.UserProfile = ""

	core := store.NewImmutableCore()
	*core.Fields() = fields
	view.core = core
	return view
}

func (f *SessionMemoryFacade) ImmutableCore() protocol.ImmutableCore {
	return f.core
}

func cloneUserPreferences(src map[string]string) map[string]string {
	if src == nil {
		return nil
	}

	cloned := make(map[string]string, len(src))
	for key, value := range src {
		cloned[key] = value
	}
	return cloned
}

func isNilInterface(value any) bool {
	if value == nil {
		return true
	}

	rv := reflect.ValueOf(value)
	switch rv.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return rv.IsNil()
	default:
		return false
	}
}
