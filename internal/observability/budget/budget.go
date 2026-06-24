package budget

import (
	"github.com/polarisagi/polaris/internal/observability/metrics"
)

// ResourceBudget 定义系统运行时的资源压力/可用性状态。
type ResourceBudget struct {
	AvailableMB   int
	IsConstrained bool
}

// BackgroundPermit checks if a background task with the given priority is allowed to run based on current cognitive pressure.
func (rb *ResourceBudget) BackgroundPermit(priority int) bool {
	if priority <= 1 {
		return true // priority 0/1 are always permitted
	}
	// For background tasks (priority >= 2), check cognitive pressure
	pressure := metrics.GetCognitivePressure()
	if pressure >= 80 {
		return false // high pressure, deny background tasks
	}
	return true
}
