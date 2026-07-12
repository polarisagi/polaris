package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/polarisagi/polaris/pkg/types"

	"github.com/polarisagi/polaris/internal/swarm/topology"
	"github.com/polarisagi/polaris/pkg/apperr"
)

const (
	escalateP2AfterMinutes = 5  // M08 §1.8: Priority=2 等待超过此值升至 1
	escalateP3AfterMinutes = 15 // M08 §1.8: Priority=3 等待超过此值强制提权
)

// Orchestrator 是多 Agent 协调的核心，封装了 Blackboard 和 AgentRegistry 的调度逻辑。
type Orchestrator struct {
	bb       *SQLiteBlackboard
	registry *AgentRegistry
	mu       sync.Mutex

	// maxAgents 在 Tier 0 环境下默认为 3
	maxAgents  int
	workers    map[string]*Worker
	evolverSvc *topology.TopologyEvolverService // 可选，nil = 禁用拓扑自演化

}

func NewOrchestrator(bb *SQLiteBlackboard, registry *AgentRegistry, maxAgents int) *Orchestrator {
	bb.SetRegistry(registry)
	return &Orchestrator{
		bb:        bb,
		registry:  registry,
		maxAgents: maxAgents,
		workers:   make(map[string]*Worker),
	}
}

// SetTopologyEvolverService 注入拓扑自演化服务（可选）。
func (o *Orchestrator) SetTopologyEvolverService(svc *topology.TopologyEvolverService) {
	o.mu.Lock()
	o.evolverSvc = svc
	o.mu.Unlock()
}

// RegisterWorker 注册存活的 Worker 以接收中心化调度任务。
func (o *Orchestrator) RegisterWorker(w *Worker) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.workers[w.agentID] = w
}

// ListenLoop 是中心化调度循环。
// 监听 EventTaskPosted，基于优先级出队，匹配最优 Agent，并通过 CAS 认领分发。
func (o *Orchestrator) ListenLoop(ctx context.Context) error {
	events, err := o.bb.Subscribe(ctx)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "failed to subscribe to blackboard", err)
	}

	// 1. 每隔一段时间也做一次后备轮询 (防事件丢失)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-events:
			if !ok {
				return nil
			}
			if (ev.Type == "task_completed" || ev.Type == "task_failed") && o.evolverSvc != nil {
				var taskType string
				var tokensIn, tokensOut int
				if err := o.bb.db.QueryRowContext(ctx, "SELECT type, tokens_input, tokens_output FROM tasks WHERE id = ?", ev.TaskID).Scan(&taskType, &tokensIn, &tokensOut); err == nil {
					o.evolverSvc.RecordOutcome("supervisor", taskType, ev.Type == "task_completed", float64(tokensIn+tokensOut))
				}
			}
			// 收到任务发布信号，进行一次调度分配
			o.dispatchPendingTasks(ctx)
		case <-ticker.C:
			// 兜底扫描
			o.dispatchPendingTasks(ctx)
		}
	}
}

// queryAgentLoads 查询数据库，返回每个 Agent 当前 claimed+running 任务数。
// 用于 FindBestAgent 负载均衡评分，使调度决策基于真实负载而非空映射。
func (o *Orchestrator) queryAgentLoads(ctx context.Context) map[string]int {
	rows, err := o.bb.db.QueryContext(ctx, `
		SELECT claimed_by, COUNT(*) FROM tasks
		WHERE status IN ('claimed', 'running') AND claimed_by IS NOT NULL
		GROUP BY claimed_by
	`)
	if err != nil {
		return map[string]int{}
	}
	defer rows.Close()
	loads := make(map[string]int)
	for rows.Next() {
		var agentID string
		var count int
		if rows.Scan(&agentID, &count) == nil {
			loads[agentID] = count
		}
	}
	return loads
}

// dispatchPendingTasks 提取 Pending 任务并尝试调度。
func (o *Orchestrator) dispatchPendingTasks(ctx context.Context) {
	o.mu.Lock()
	defer o.mu.Unlock()

	// 1. 获取所有 pending 任务，按 priority ASC, created_at ASC 排序 (含动态优先级计算)
	query := fmt.Sprintf(`
		SELECT task_id, type, priority,
			MAX(0,
				priority
				- CASE
					WHEN priority >= 3
						 AND CAST((julianday('now') - julianday(created_at)) * 1440 AS INTEGER) >= %d
					  THEN 2
					WHEN priority >= 2
						 AND CAST((julianday('now') - julianday(created_at)) * 1440 AS INTEGER) >= %d
					  THEN 1
					ELSE 0
				  END
			) AS eff_priority
		FROM tasks
		WHERE status = 'pending'
		ORDER BY eff_priority ASC, created_at ASC
	`, escalateP3AfterMinutes, escalateP2AfterMinutes)

	rows, err := o.bb.db.QueryContext(ctx, query)
	if err != nil {
		return
	}
	defer rows.Close()

	var pendingTasks []types.TaskEntry
	for rows.Next() {
		var task types.TaskEntry
		var effPriority int
		if err := rows.Scan(&task.ID, &task.Type, &task.Priority, &effPriority); err == nil {
			if effPriority < task.Priority {
				slog.Info("orchestrator: task priority escalated", "task_id", task.ID, "from", task.Priority, "to", effPriority)
			}
			pendingTasks = append(pendingTasks, task)
		}
	}
	rows.Close()

	if len(pendingTasks) == 0 {
		return
	}

	// 2. 依次尝试分发
	for _, task := range pendingTasks {
		// 并发上限控制 (Tier 0 极简控制: 直接查当前 running/claimed 数量)
		var activeCount int
		err := o.bb.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks WHERE status IN ('claimed', 'running')`).Scan(&activeCount)
		if err == nil && activeCount >= o.maxAgents {
			// 达到系统并发上限，暂缓分发后续任务
			break
		}

		// capabilities 检查：task.Type 作为主能力标识，空则不限制
		requiredCaps := []string{task.Type}
		if task.Type == "" {
			requiredCaps = nil
		}

		// 3. 查询各 Agent 当前活跃任务数，作为负载权重传入 FindBestAgent
		currentLoads := o.queryAgentLoads(ctx)

		agentHandle, err := o.registry.FindBestAgent(requiredCaps, currentLoads, map[string]AgentStats{})
		if err != nil {
			// 找不到合适的 Agent (可能是全忙或能力不匹配)，继续看下一个任务
			continue
		}

		// 4. 尝试 CAS Claim
		agentID := agentHandle.Card.Name
		success, _ := o.bb.ClaimTask(ctx, task.ID, agentID) // 简化：用 Name 作为 ID
		if success {
			slog.Info("orchestrator: task claimed", "task_id", task.ID, "agent", agentID)

			// 投递给对应 Agent 的 Channel
			if worker, ok := o.workers[agentID]; ok {
				select {
				case worker.TaskPushChan <- task.ID:
					slog.Debug("orchestrator: task pushed to worker", "task_id", task.ID, "agent", agentID)
				default:
					slog.Warn("orchestrator: worker push channel full", "agent", agentID, "task_id", task.ID, "err", apperr.New(apperr.CodeInternal, "log event"))
				}
			} else {
				slog.Warn("orchestrator: worker not found", "agent", agentID, "task_id", task.ID, "err", apperr.New(apperr.CodeInternal, "log event"))
			}
		}
	}
}
