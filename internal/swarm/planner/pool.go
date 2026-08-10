package planner

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/polarisagi/polaris/pkg/types"

	"github.com/polarisagi/polaris/internal/llm/safecall"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/concurrent"
)

type SandboxExecutor interface {
	Execute(ctx context.Context, cmd string, args []string, workDir string, timeout time.Duration) ([]byte, error)
}

// WorkspaceStager 是 workerEngineA 落盘候选补丁供沙箱编译/测试所需的最小接口
// （HE-3：接口在调用方定义，不反向依赖 internal/vfs 具体类型）。真实实现为
// *vfs.WorkspaceManager.StageEphemeralFile——落盘目标纳入 WorkspaceManager 管辖
// 的 rootDir 配额/GC 体系，而非裸 os.MkdirTemp/os.WriteFile 直接触碰宿主机
// 系统级 /tmp（脱离 VFS 隔离边界，见 workerEngineA 原 TODO(HE-3)）。
type WorkspaceStager interface {
	StageEphemeralFile(namespace, filename string, data []byte) (absPath string, cleanup func(), err error)
}

// PlannerPool 管理多个并发的思考流，并将最佳结果（通过耳语）汇报给主脑。
type PlannerPool struct {
	goal        string
	taskType    string
	whisperChan chan<- protocol.MemoryWhisper // 结果返回通道
	provider    protocol.Provider
	sandbox     SandboxExecutor
	workspace   WorkspaceStager
	decomposer  *TaskDecomposer
}

// SetWorkspace 注入 WorkspaceStager（通常为 *vfs.WorkspaceManager），供
// workerEngineA 落盘候选补丁到沙箱可执行的真实路径。未注入时 workerEngineA
// 直接跳过编译评分（返回零分工作结果），不再回退到裸文件系统调用。
func (p *PlannerPool) SetWorkspace(ws WorkspaceStager) {
	p.workspace = ws
}

// NewPlannerPool 创建 PlannerPool。toolLookup 可传 nil（decomposer 跳过白名单校验）。
func NewPlannerPool(goal, taskType string, provider protocol.Provider, whisperChan chan<- protocol.MemoryWhisper, toolLookup ToolLookup) *PlannerPool {
	return &PlannerPool{
		goal:        goal,
		taskType:    taskType,
		whisperChan: whisperChan,
		provider:    provider,
		decomposer:  NewTaskDecomposer(provider, toolLookup), // 自动注入
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

	// 收集所有结果，选得分最高的推送。
	// 不能用零值 workerResult{} 做初始 best 再靠 `res.score > best.score` 筛选——
	// workerEngineA 在 p.sandbox 未注入（compileScore 恒为 0.0）等场景下，所有
	// worker 的 score 都等于零值 best.score，`0.0 > 0.0` 恒假，导致合法的
	// （score=0 但 content 非空的）结果被整体静默丢弃，best.content 全程保持
	// ""，最终 whisper 完全不推送。用 hasResult 标记首个结果，之后按分数择优。
	var best workerResult
	hasResult := false
	for res := range resultChan {
		if res.content == "" {
			continue
		}
		if !hasResult || res.score > best.score {
			best = res
			hasResult = true
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

	prompt := fmt.Sprintf("Generate the Go code patch only.\n\n<goal>\n%s\n</goal>\n<task_type>%s</task_type>\n<constraint>\n%s\n</constraint>", p.goal, p.taskType, systemPrompt)
	req := &types.InferRequest{
		Messages: []types.Message{
			{Role: "user", Content: prompt},
		},
		Temperature: []float64{0.2, 0.7, 1.2}[workerID],
		Model:       "reasoning",
	}

	resp, err := safecall.Infer(ctx, p.provider, req.Messages, types.WithMaxTokens(req.MaxTokens))
	if err != nil || resp == nil || len(resp.Content) == 0 {
		return
	}
	patchStr := resp.Content

	if p.workspace == nil {
		// 未注入 WorkspaceStager（如 boot_agent.go 未接线 vfs.WorkspaceManager 的
		// 测试/降级场景）：不再回退到裸 os.MkdirTemp/os.WriteFile 触碰宿主机系统级
		// /tmp，直接跳过编译评分。
		return
	}

	namespace := fmt.Sprintf("planner-pool-worker-%d", workerID)
	testFile, cleanup, err := p.workspace.StageEphemeralFile(namespace, "patch_gen.go", []byte(patchStr))
	if err != nil {
		return
	}
	defer cleanup()

	// go build/test 的目标目录取落盘文件的父目录（WorkspaceManager rootDir 下
	// 的真实路径），沙箱工作目录同取该目录——两者此前分别为 tmpDir（构建目标）
	// 与 os.TempDir()（sandbox workDir），落盘迁移到 WorkspaceManager 后统一为
	// 同一目录，语义不变（sandbox.Execute 仍以 "go build <dir>" 形式指定目标）。
	tmpDir := filepath.Dir(testFile)
	wd := tmpDir

	buildCtx, cancel1 := context.WithTimeout(ctx, 30*time.Second)
	defer cancel1()

	var compileScore = 0.0

	if p.sandbox != nil {
		_, buildErr := p.sandbox.Execute(buildCtx, "go", []string{"build", tmpDir}, wd, 30*time.Second)
		if buildErr == nil {
			testCtx, cancel2 := context.WithTimeout(ctx, 20*time.Second)
			defer cancel2()

			out, testErr := p.sandbox.Execute(testCtx, "go", []string{"test", "-json", "-timeout", "20s", tmpDir}, wd, 20*time.Second)
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
		prompt := fmt.Sprintf("Create a detailed plan.\n<goal>\n%s\n</goal>\n<task_type>%s</task_type>", p.goal, p.taskType)
		req := &types.InferRequest{
			Messages: []types.Message{
				{Role: "user", Content: prompt},
			},
			Temperature: t,
			Model:       "reasoning",
		}

		resp, err := safecall.Infer(ctx, p.provider, req.Messages, types.WithMaxTokens(req.MaxTokens))
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
