package planner

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/polarisagi/polaris/internal/agent/dag"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// subTask 是 LLM 返回的单个子任务 JSON 节点。
type subTask struct {
	ID          string   `json:"id"`          // 唯一标识（如 "t1", "t2"）
	Name        string   `json:"name"`        // 简短名称
	Description string   `json:"description"` // 完整说明
	ToolName    string   `json:"tool_name"`   // 调用的工具名（空时用通用 "agent:run"）
	Args        any      `json:"args"`        // 工具参数（任意 JSON）
	DependsOn   []string `json:"depends_on"`  // 前驱 ID 列表（空=可立即执行）
}

// decompositionResponse LLM 返回的完整 JSON 结构。
type decompositionResponse struct {
	Tasks []subTask `json:"tasks"`
}

// TaskDecomposer 将高层目标分解为可执行的 DAG 节点列表。
// 架构文档: docs/arch/M08-Swarm-Orchestration.md §8.2 Planner
type TaskDecomposer struct {
	provider protocol.Provider
}

// NewTaskDecomposer 构造分解器。
func NewTaskDecomposer(provider protocol.Provider) *TaskDecomposer {
	return &TaskDecomposer{provider: provider}
}

// Decompose 将 goal 分解为 []dag.ExecNode，最多 8 个子任务。
// 分解失败时返回单节点 fallback（不报错），保证调用方始终拿到可执行计划。
func (d *TaskDecomposer) Decompose(ctx context.Context, goal string) ([]dag.ExecNode, error) {
	if d.provider == nil {
		return d.fallbackSingleNode(goal), nil
	}

	nodes, err := d.decomposeLLM(ctx, goal)
	if err != nil {
		slog.Warn("task_decomposer: LLM decomposition failed, using single-node fallback",
			"goal", goal, "err", err)
		return d.fallbackSingleNode(goal), nil
	}
	if len(nodes) == 0 {
		return d.fallbackSingleNode(goal), nil
	}
	return nodes, nil
}

// decomposeLLM 调用 LLM，强制返回 JSON 数组格式。
func (d *TaskDecomposer) decomposeLLM(ctx context.Context, goal string) ([]dag.ExecNode, error) {
	systemPrompt := `You are a task decomposition engine. 
Given a high-level goal, break it into at most 8 concrete sub-tasks.
Return ONLY a JSON object with a "tasks" array. Each task must have:
  - id: unique short string ("t1", "t2", ...)
  - name: short title
  - description: what to do
  - tool_name: tool to call (e.g. "builtin:web_search", "builtin:file_write", "agent:run")
  - args: JSON object with tool arguments (use {"goal": description} for agent:run)
  - depends_on: array of task ids that must complete first (empty if none)
Ensure no circular dependencies. Output ONLY valid JSON, no markdown.`

	userPrompt := fmt.Sprintf("Decompose this goal into sub-tasks:\n%s", goal)

	resp, err := d.provider.Infer(ctx, []types.Message{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: userPrompt},
	}, types.WithMaxTokens(1024))
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "task_decomposer: LLM infer failed", err)
	}

	content := strings.TrimSpace(resp.Content)
	content = strings.TrimPrefix(content, "```json")
	content = strings.TrimPrefix(content, "```")
	content = strings.TrimSuffix(content, "```")
	content = strings.TrimSpace(content)

	var decomp decompositionResponse
	if err := json.Unmarshal([]byte(content), &decomp); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "task_decomposer: parse JSON failed", err)
	}

	return d.mapToExecNodes(decomp.Tasks)
}

// mapToExecNodes 将 []subTask 映射为 []dag.ExecNode。
func (d *TaskDecomposer) mapToExecNodes(tasks []subTask) ([]dag.ExecNode, error) {
	if len(tasks) == 0 {
		return nil, apperr.New(apperr.CodeInternal, "task_decomposer: LLM returned empty task list")
	}
	// 上限 8 个子任务（防 Prompt 爆炸）
	if len(tasks) > 8 {
		tasks = tasks[:8]
	}

	nodes := make([]dag.ExecNode, 0, len(tasks))
	for _, t := range tasks {
		toolName := t.ToolName
		if toolName == "" {
			toolName = "agent:run"
		}

		// 将 args 序列化为 JSON bytes
		var argsBytes []byte
		if t.Args != nil {
			var err error
			argsBytes, err = json.Marshal(t.Args)
			if err != nil {
				argsBytes = []byte("{}")
			}
		} else {
			argsBytes, _ = json.Marshal(map[string]string{
				"goal": t.Description,
			})
		}

		nodes = append(nodes, dag.ExecNode{
			ID:        t.ID,
			ToolName:  toolName,
			Args:      argsBytes,
			DependsOn: t.DependsOn,
			MaxRetry:  1,
		})
	}
	return nodes, nil
}

// fallbackSingleNode 分解失败时返回包裹原始 goal 的单节点 DAG。
func (d *TaskDecomposer) fallbackSingleNode(goal string) []dag.ExecNode {
	args, _ := json.Marshal(map[string]string{"goal": goal})
	return []dag.ExecNode{
		{
			ID:        "t1",
			ToolName:  "agent:run",
			Args:      args,
			DependsOn: nil,
			MaxRetry:  1,
		},
	}
}
