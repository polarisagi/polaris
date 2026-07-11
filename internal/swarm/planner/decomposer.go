package planner

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/polarisagi/polaris/internal/llm/safecall"
	"github.com/polarisagi/polaris/internal/prompt/templates"
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

// ToolLookup 是 TaskDecomposer 校验 LLM 生成的 tool_name 是否合法所需的最小依赖
// （消费方定义，符合 R1.4）。由 dispatch.Dispatcher 满足（Dispatcher.Lookup 签名与此一致）。
type ToolLookup interface {
	Lookup(name string) (types.Tool, error)
}

// agentRunSentinel 是 decomposer.tmpl 里约定的特殊 tool_name，代表"递归调用子 Agent
// 处理该子任务"，不是 ToolRegistry 里注册的真实工具，白名单校验必须放行这个哨兵值。
const agentRunSentinel = "agent:run"

// TaskDecomposer 将高层目标分解为可执行的 DAG 节点列表。
// 架构文档: docs/arch/M08-Multi-Agent-Orchestrator.md §8.2 Planner
type TaskDecomposer struct {
	provider protocol.Provider
	// toolLookup 用于校验 LLM 生成的 tool_name 是否在工具目录中注册（GR-7-002 修复）。
	// 可为 nil（测试/未接入工具目录场景），此时跳过白名单校验，行为与修复前一致。
	toolLookup ToolLookup
}

// NewTaskDecomposer 构造分解器。toolLookup 可传 nil（跳过白名单校验）。
func NewTaskDecomposer(provider protocol.Provider, toolLookup ToolLookup) *TaskDecomposer {
	return &TaskDecomposer{provider: provider, toolLookup: toolLookup}
}

// Decompose 将 goal 分解为 []protocol.ExecNode，最多 8 个子任务。
// 分解失败时返回单节点 fallback（不报错），保证调用方始终拿到可执行计划。
func (d *TaskDecomposer) Decompose(ctx context.Context, goal string) ([]protocol.ExecNode, error) {
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
func (d *TaskDecomposer) decomposeLLM(ctx context.Context, goal string) ([]protocol.ExecNode, error) {
	systemPrompt, err := templates.Render("decomposer.tmpl", nil)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "task_decomposer: render template failed", err)
	}

	userPrompt := fmt.Sprintf("Decompose this goal into sub-tasks:\n%s", goal)

	resp, err := safecall.Infer(ctx, d.provider, []types.Message{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: userPrompt},
	}, types.WithMaxTokens(1024))
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "task_decomposer: LLM infer failed", err)
	}
	if resp == nil {
		return nil, apperr.New(apperr.CodeInternal, "task_decomposer: empty response")
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

// mapToExecNodes 将 []subTask 映射为 []protocol.ExecNode。
func (d *TaskDecomposer) mapToExecNodes(tasks []subTask) ([]protocol.ExecNode, error) {
	if len(tasks) == 0 {
		return nil, apperr.New(apperr.CodeInternal, "task_decomposer: LLM returned empty task list")
	}
	// 上限 8 个子任务（防 Prompt 爆炸）
	if len(tasks) > 8 {
		tasks = tasks[:8]
	}

	nodes := make([]protocol.ExecNode, 0, len(tasks))
	for _, t := range tasks {
		toolName := t.ToolName
		if toolName == "" {
			toolName = agentRunSentinel
		}

		// 白名单校验（GR-7-002）：LLM 生成的 tool_name 若不在工具目录中注册，
		// 在规划阶段就拦截，不放到 DAG 执行阶段才失败。agentRunSentinel 是
		// decomposer.tmpl 约定的特殊值（递归子 Agent），不经过工具目录，豁免校验。
		if d.toolLookup != nil && toolName != agentRunSentinel {
			if _, err := d.toolLookup.Lookup(toolName); err != nil {
				return nil, apperr.New(apperr.CodeInvalidInput,
					"task_decomposer: LLM produced unregistered tool name: "+toolName)
			}
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

		nodes = append(nodes, protocol.ExecNode{
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
func (d *TaskDecomposer) fallbackSingleNode(goal string) []protocol.ExecNode {
	args, _ := json.Marshal(map[string]string{"goal": goal})
	return []protocol.ExecNode{
		{
			ID:        "t1",
			ToolName:  "agent:run",
			Args:      args,
			DependsOn: nil,
			MaxRetry:  1,
		},
	}
}
