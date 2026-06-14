package eval

import (
	"sync"
)

// SamplingMonitor 跟踪每个 session 的 Temperature/TopP 漂移情况
type SamplingMonitor struct {
	mu       sync.Mutex
	sessions map[string][]SamplingRecord
}

type SamplingRecord struct {
	Temperature float64
	TopP        float64
}

// NewSamplingMonitor 创建一个新的 SamplingMonitor
func NewSamplingMonitor() *SamplingMonitor {
	return &SamplingMonitor{
		sessions: make(map[string][]SamplingRecord),
	}
}

// Record 记录单个会话的采样参数
func (m *SamplingMonitor) Record(sessionID string, temp float64, topP float64) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.sessions[sessionID] = append(m.sessions[sessionID], SamplingRecord{
		Temperature: temp,
		TopP:        topP,
	})
}
