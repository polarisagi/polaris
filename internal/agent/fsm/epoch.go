package fsm

import (
	"crypto/sha256"
	"encoding/hex"
	"sync/atomic"

	"github.com/polarisagi/polaris/pkg/types"
)

// epochTracker 跟踪上下文指纹，指纹变化时递增 epoch（M05 §11）。并发安全。
type epochTracker struct {
	lastFP atomic.Value // string
	epoch  atomic.Int64
}

func NewEpochTracker() *epochTracker {
	t := &epochTracker{}
	t.epoch.Store(1)
	return t
}

// check 对消息序列计算指纹；与上次不同则递增 epoch 并返回新值。
func (t *epochTracker) check(msgs []types.Message) int64 {
	h := sha256.New()
	for _, m := range msgs {
		_, _ = h.Write([]byte(m.Role))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(m.Content))
		_, _ = h.Write([]byte{0})
	}
	fp := hex.EncodeToString(h.Sum(nil))
	if last, ok := t.lastFP.Load().(string); ok && last == fp {
		return t.epoch.Load()
	}
	t.lastFP.Store(fp)
	return t.epoch.Add(1)
}
