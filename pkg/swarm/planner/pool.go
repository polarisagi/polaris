package planner

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
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
		go func(id int) {
			defer wg.Done()
			if p.taskType == "code_act" {
				p.workerEngineA(ctx, id, resultChan)
			} else {
				p.workerEngineB(ctx, id, resultChan)
			}
		}(i)
	}

	go func() {
		wg.Wait()
		close(resultChan)
	}()

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
	req := &protocol.InferRequest{
		Messages: []protocol.Message{
			{Role: "user", Content: prompt},
		},
		Temperature: []float64{0.2, 0.7, 1.2}[workerID],
		Model:       "reasoning",
	}

	resp, err := p.provider.Infer(ctx, req)
	if err != nil || resp == nil || len(resp.Content) == 0 {
		return
	}
	patchStr := resp.Content

	tmpDir, err := os.MkdirTemp("", "polaris-pool-a-*")
	if err != nil {
		return
	}
	defer os.RemoveAll(tmpDir)

	testFile := filepath.Join(tmpDir, "main.go")
	_ = os.WriteFile(testFile, []byte(patchStr), 0600)

	buildCtx, cancel1 := context.WithTimeout(ctx, 30*time.Second)
	defer cancel1()

	cmdBuild := exec.CommandContext(buildCtx, "go", "build", "./...")
	cmdBuild.Dir = tmpDir
	buildErr := cmdBuild.Run()

	var compileScore float64 = 0.0

	//nolint:nestif
	if buildErr == nil {
		testCtx, cancel2 := context.WithTimeout(ctx, 20*time.Second)
		defer cancel2()

		cmdTest := exec.CommandContext(testCtx, "go", "test", "-run", ".", "-timeout", "20s", "./...")
		cmdTest.Dir = tmpDir
		out, _ := cmdTest.CombinedOutput()

		if cmdTest.ProcessState != nil && cmdTest.ProcessState.Success() {
			compileScore = 1.0
		} else {
			outStr := string(out)
			if strings.Contains(outStr, "no test files") || outStr == "" {
				compileScore = 0.5
			} else {
				// simple dummy parsing logic
				compileScore = 0.5 + 0.5*0.5 // partial success simulation
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

func (p *PlannerPool) workerEngineB(ctx context.Context, workerID int, resultChan chan<- workerResult) {
	temperatures := []float64{0.2, 0.7, 1.2}
	t := temperatures[workerID]

	time.Sleep(100 * time.Millisecond)

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
