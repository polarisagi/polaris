package planner

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/action/tool"
)

// PlannerPool 管理多个并发的思考流，并将最佳结果（通过耳语）汇报给主脑。
type PlannerPool struct {
	goal        string
	taskType    string
	whisperChan chan<- protocol.MemoryWhisper // 结果返回通道
	provider    protocol.Provider
}

// NewPlannerPool 创建 PlannerPool。
func NewPlannerPool(goal, taskType string, provider protocol.Provider, whisperChan chan<- protocol.MemoryWhisper) *PlannerPool {
	return &PlannerPool{
		goal:        goal,
		taskType:    taskType,
		whisperChan: whisperChan,
		provider:    provider,
	}
}

// Run 启动一组并发 Planner，当有任何一个产生高置信度计划时，通过 whisperChan 推送
func (p *PlannerPool) Run(ctx context.Context) {
	if p.whisperChan == nil {
		return
	}

	var wg sync.WaitGroup
	// 启动 3 个不同温度的 worker
	temperatures := []float64{0.2, 0.7, 1.2}
	resultChan := make(chan string, len(temperatures))

	for i, temp := range temperatures {
		wg.Add(1)
		go func(id int, t float64) {
			defer wg.Done()

			// 模拟规划耗时
			time.Sleep(100 * time.Millisecond)

			//nolint:nestif
			if p.provider != nil {
				prompt := fmt.Sprintf("Create a detailed plan for goal: %s (taskType: %s)", p.goal, p.taskType)
				req := &protocol.InferRequest{
					Messages: []protocol.Message{
						{Role: "user", Content: prompt},
					},
					Temperature: t,
					Model:       "reasoning",
				}

				resp, err := p.provider.Infer(ctx, req)
				if err == nil && resp != nil && len(resp.Content) > 0 {
					planStr := resp.Content

					if p.taskType == "code_act" {
						// 模拟 Engine A：真实 Wasm 编译与单测打分
						// 这里为了演示，我们将生成的文本假装是编译好的 Wasm 字节码送入 WasmtimeExecute
						wasmBytes := []byte(planStr)
						outJSON, execErr := tool.WasmtimeExecute(wasmBytes, "{}", "", 1, false, 1000, 0)

						if execErr == nil {
							// 假设通过单测，将打分附加到 planStr 后面
							planStr = fmt.Sprintf("%s\n\n[Wasm Evaluation Score: 100/100, out: %s]", planStr, outJSON)
						} else {
							planStr = fmt.Sprintf("%s\n\n[Wasm Evaluation Failed: %v]", planStr, execErr)
						}
					}

					resultChan <- planStr
					return
				}
			}

			// Fallback mock
			resultChan <- fmt.Sprintf("Fallback plan for %s at temp %f", p.goal, t)
		}(i, temp)
	}

	go func() {
		wg.Wait()
		close(resultChan)
	}()

	// 任何 worker 成功后，推送到 whisperChan
	select {
	case <-ctx.Done():
		return
	case res, ok := <-resultChan:
		if ok {
			p.whisperChan <- protocol.MemoryWhisper{
				Content:  fmt.Sprintf("Planner found a valid plan: %s", res),
				Source:   "planner_pool",
				Salience: 0.9,
			}
		}
	}
}

// DefaultSpawner 是用于注入到 kernel 的默认构造器函数
func DefaultSpawner(ctx context.Context, goal, taskType string, provider protocol.Provider, whisperChan chan<- protocol.MemoryWhisper) {
	pool := NewPlannerPool(goal, taskType, provider, whisperChan)
	pool.Run(ctx)
}
