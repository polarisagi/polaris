package planner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/polarisagi/polaris/pkg/types"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/concurrent"
)

type SandboxExecutor interface {
	Execute(ctx context.Context, cmd string, args []string, workDir string, timeout time.Duration) ([]byte, error)
}

// PlannerPool 管理多个并发的思考流，并将最佳结果（通过耳语）汇报给主脑。
type PlannerPool struct {
	goal        string
	taskType    string
	whisperChan chan<- protocol.MemoryWhisper // 结果返回通道
	provider    protocol.Provider
	sandbox     SandboxExecutor
	decomposer  *TaskDecomposer
}

// NewPlannerPool 创建 PlannerPool。
func NewPlannerPool(goal, taskType string, provider protocol.Provider, whisperChan chan<- protocol.MemoryWhisper) *PlannerPool {
	return &PlannerPool{
		goal:        goal,
		taskType:    taskType,
		whisperChan: whisperChan,
		provider:    provider,
		decomposer:  NewTaskDecomposer(provider), // 自动注入
	}
}

type workerResult struct {
	score   float64
	content string
}

// Run 启动一组并发 Planner，当有任何一个产生高置信度计划时，通过 whisperChan 推送
func (p *PlannerPool) Run(ctx context.Context) {
	if p.whisperChan == nil {
		return
	}

	workerCount := 3
	resultChan := make(chan workerResult, workerCount)
	var wg sync.WaitGroup

	for i := 0; i < workerCount; i++ {
		wg.Add(1)
		id := i
		concurrent.SafeGo(ctx, fmt.Sprintf("planner_worker_%d", id), func(ctx context.Context) {
			defer wg.Done()
			if p.taskType == "code_act" {
				p.workerEngineA(ctx, id, resultChan)
			} else {
				p.workerEngineB(ctx, id, resultChan)
			}
		})
	}

	concurrent.SafeGo(ctx, "planner_waiter", func(_ context.Context) {
		wg.Wait()
		close(resultChan)
	})

	// 收集所有结果，选得分最高的推送
	var best workerResult
	for res := range resultChan {
		if res.score > best.score {
			best = res
		}
	}

	if best.content != "" {
		select {
		case p.whisperChan <- protocol.MemoryWhisper{
			Content:  best.content,
			Source:   "planner_pool",
			Salience: best.score,
		}:
		case <-ctx.Done():
		}
	}
}

func (p *PlannerPool) workerEngineA(ctx context.Context, workerID int, resultChan chan<- workerResult) {
	if p.provider == nil {
		return
	}

	systemPrompt := ""
	switch workerID {
	case 0:
		systemPrompt = "最小修改，保持现有风格"
	case 1:
		systemPrompt = "正确性优先，允许重写"
	case 2:
		systemPrompt = "性能优先，可引入新依赖"
	}

	prompt := fmt.Sprintf("Goal: %s\nTaskType: %s\nConstraint: %s\nGenerate the Go code patch only.", p.goal, p.taskType, systemPrompt)
	req := &types.InferRequest{
		Messages: []types.Message{
			{Role: "user", Content: prompt},
		},
		Temperature: []float64{0.2, 0.7, 1.2}[workerID],
		Model:       "reasoning",
	}

	resp, err := p.provider.Infer(ctx, req.Messages, types.WithMaxTokens(req.MaxTokens))
	if err != nil || resp == nil || len(resp.Content) == 0 {
		return
	}
	patchStr := resp.Content

	wd, err := os.Getwd()
	if err != nil {
		return
	}

	tmpDir, err := os.MkdirTemp(".", "planner-pool-*")
	if err != nil {
		return
	}
	defer os.RemoveAll(tmpDir)

	testFile := filepath.Join(tmpDir, "patch_gen.go")
	_ = os.WriteFile(testFile, []byte(patchStr), 0600)

	buildCtx, cancel1 := context.WithTimeout(ctx, 30*time.Second)
	defer cancel1()

	relDir := "./" + filepath.Base(tmpDir)

	var compileScore float64 = 0.0

	if p.sandbox != nil {
		_, buildErr := p.sandbox.Execute(buildCtx, "go", []string{"build", relDir}, wd, 30*time.Second)
		if buildErr == nil {
			testCtx, cancel2 := context.WithTimeout(ctx, 20*time.Second)
			defer cancel2()

			out, testErr := p.sandbox.Execute(testCtx, "go", []string{"test", "-json", "-timeout", "20s", relDir}, wd, 20*time.Second)
			if testErr == nil {
				compileScore = 1.0
			} else {
				compileScore = parseTestScore(out)
			}
		}
	}

	preview := patchStr
	if len(preview) > 200 {
		preview = preview[:200]
	}
	content := fmt.Sprintf("[PLANNER_ENGINE_A] score=%.2f patch=%s", compileScore, preview)

	resultChan <- workerResult{
		score:   compileScore,
		content: content,
	}
}

// 解析 go test -json 输出，统计具体 Test 的 PASS 和 FAIL 数量
func parseTestScore(output []byte) float64 {
	out := string(output)
	if strings.Contains(out, "no test files") || strings.TrimSpace(out) == "" {
		return 0.5 // 编译成功但无测试，得中等分
	}

	lines := strings.Split(out, "\n")
	var pass, fail int
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		// 寻找 JSON 格式输出中对 Test 级别的事件
		if strings.Contains(line, `"Action":"pass"`) && strings.Contains(line, `"Test":`) {
			pass++
		} else if strings.Contains(line, `"Action":"fail"`) && strings.Contains(line, `"Test":`) {
			fail++
		}
	}

	total := pass + fail
	if total == 0 {
		return 0.5
	}
	return 0.5 + 0.5*float64(pass)/float64(total)
}

func (p *PlannerPool) workerEngineB(ctx context.Context, workerID int, resultChan chan<- workerResult) {
	// workerID==0 使用结构化分解，其他 worker 继续原有路径（多样性）
	if workerID == 0 && p.decomposer != nil {
		nodes, err := p.decomposer.Decompose(ctx, p.goal)
		if err == nil && len(nodes) > 0 {
			dagJSON, _ := json.Marshal(nodes)
			resultChan <- workerResult{
				score:   0.95, // 结构化分解得高分
				content: fmt.Sprintf("[DECOMPOSED_DAG] %s", string(dagJSON)),
			}
			return
		}
	}

	temperatures := []float64{0.2, 0.7, 1.2}
	t := temperatures[workerID]

	time.Sleep(100 * time.Millisecond)

	if p.provider != nil {
		prompt := fmt.Sprintf("Create a detailed plan for goal: %s (taskType: %s)", p.goal, p.taskType)
		req := &types.InferRequest{
			Messages: []types.Message{
				{Role: "user", Content: prompt},
			},
			Temperature: t,
			Model:       "reasoning",
		}

		resp, err := p.provider.Infer(ctx, req.Messages, types.WithMaxTokens(req.MaxTokens))
		if err == nil && resp != nil && len(resp.Content) > 0 {
			resultChan <- workerResult{
				score:   0.9,
				content: resp.Content,
			}
			return
		}
	}

	resultChan <- workerResult{
		score:   0.1,
		content: fmt.Sprintf("Fallback plan for %s at temp %f", p.goal, t),
	}
}

// DefaultSpawner 是用于注入到 kernel 的默认构造器函数
func DefaultSpawner(ctx context.Context, goal, taskType string, provider protocol.Provider, whisperChan chan<- protocol.MemoryWhisper) {
	pool := NewPlannerPool(goal, taskType, provider, whisperChan)
	pool.Run(ctx)
}
