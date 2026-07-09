package automation

import (
	"github.com/polarisagi/polaris/internal/protocol"
)

// AutomationFacade automation 包对外统一接口（任务调度 + HITL 审批）。
type AutomationFacade = protocol.AutomationFacade

// var _ protocol.AutomationFacade = (*SQLiteScheduler)(nil)
