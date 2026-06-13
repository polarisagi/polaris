package swarm

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
)

// ReflexionEngine 是 AgentHER (Hindsight Experience Replay) 的核心引擎。
// 失败路径：Reflexion 三步（原有实现）。
// 成功路径：success-after-replan → 完整轨迹提炼 → SurrealDB（见 ReplaySuccess 方法，待实现）。

// FailureClass 区分失败类型，与 self_improve/engine.go 保持语义一致。
// Uncontrollable 失败（网络/Provider 故障）不计入 MEMF，防止污染失败记忆池。
type FailureClass string

const (
	FailureLogic          FailureClass = "logic"
	FailureControllable   FailureClass = "controllable"
	FailureUncontrollable FailureClass = "uncontrollable"
)

// TaskResult 单次任务执行结果（Reflexion 输入）。
type TaskResult struct {
	TaskID       string
	Success      bool
	FailureClass FailureClass
	Output       []byte
}

// IsUncontrollable 判断失败是否为基础设施故障（不应更新 MEMF）。
func (r *TaskResult) IsUncontrollable() bool {
	return !r.Success && r.FailureClass == FailureUncontrollable
}

//
// 三步流程：
//   步骤1 失败分析 → cause（根本原因）
//   步骤2 反事实推理 → counterfactual（改变了 X 就能成功？）
//   步骤3 生成 Heuristic → 写入 HeuristicsMemory + 写入 MEMF（排除 Uncontrollable）

// Step 表示任务轨迹中的一步。
type Step struct {
	Index     int    `json:"index"`
	Action    string `json:"action"`
	Reasoning string `json:"reasoning"`
	Result    string `json:"result"`
	Success   bool   `json:"success"`
}

// Reflection 单次反思结果。
type Reflection struct {
	TaskID             string `json:"task_id"`
	Cause              string `json:"cause"`               // 失败根本原因
	Counterfactual     string `json:"counterfactual"`      // 反事实推理
	GeneratedHeuristic string `json:"generated_heuristic"` // 提炼出的启发式规则
	MEMFRecordID       string `json:"memf_record_id,omitempty"`
	CreatedAt          int64  `json:"created_at"`
}

// ReflexionEngine 执行反思闭环。
// llmInfer 是 LLM 推理接口（依赖注入，可 mock）。
type ReflexionEngine struct {
	memf       *FallacyMemoryPool
	heuristics *HeuristicsMemory
	// llmInfer 允许调用方注入真实的 LLM 推理函数；nil 则使用 MVP 规则引擎。
	llmInfer func(ctx context.Context, prompt string) (string, error)
	// heuristicCh 非 nil 时，步骤3完成后将 AvoidRule 发布给 self_improve.Engine 内环。
	heuristicCh chan<- protocol.HeuristicGeneratedPayload
	// db 和 surreal 用于回写 AgentHER 轨迹
	db      *sql.DB
	surreal SurrealWriter
}

// SurrealWriter defines the subset of SurrealDB operations needed.
type SurrealWriter interface {
	FTSIndex(docID, text string) error
	GraphRelate(fromID, edgeType, toID string, weight float64) error
}

// NewReflexionEngine 创建反思引擎。
func NewReflexionEngine(
	memf *FallacyMemoryPool,
	heuristics *HeuristicsMemory,
	llmInfer func(ctx context.Context, prompt string) (string, error),
) *ReflexionEngine {
	return &ReflexionEngine{
		memf:       memf,
		heuristics: heuristics,
		llmInfer:   llmInfer,
	}
}

// InjectDependencies 为 AgentHER 注入外部依赖。
func (re *ReflexionEngine) InjectDependencies(db *sql.DB, surreal SurrealWriter) {
	re.db = db
	re.surreal = surreal
}

// SetHeuristicChannel 注入事件发布通道（可选；nil 时不发布，HE-Rule-3）。
func (re *ReflexionEngine) SetHeuristicChannel(ch chan<- protocol.HeuristicGeneratedPayload) {
	re.heuristicCh = ch
}

// Reflect 对失败任务执行三步反思，返回 Reflection。
// 若任务为 Uncontrollable 失败（网络中断/Provider 崩溃），跳过 MEMF 写入。
func (re *ReflexionEngine) Reflect(
	ctx context.Context,
	taskID string,
	taskType string,
	result *TaskResult,
	trajectory []Step,
	replanCount int,
) (*Reflection, error) {
	if result == nil || result.Success {
		// AgentHER：如果是经过 replan 后才成功（replanCount > 0），
		// 这是宝贵的"犯错→探索→纠偏"轨迹，写入 SurrealDB 技能库
		if result != nil && result.Success && replanCount > 0 && len(trajectory) > 0 {
			return re.replaySuccess(ctx, taskID, taskType, trajectory, replanCount)
		}
		return nil, nil
	}

	ref := &Reflection{
		TaskID:    taskID,
		CreatedAt: time.Now().Unix(),
	}

	// 步骤 1 — 失败分析
	causePrompt := buildCausePrompt(taskType, trajectory)
	cause, err := re.infer(ctx, causePrompt)
	if err != nil {
		cause = inferCauseFromTrajectory(trajectory) // fallback：规则推断
	}
	ref.Cause = cause

	// 步骤 2 — 反事实推理
	cfPrompt := buildCounterfactualPrompt(taskType, trajectory, cause)
	cf, err := re.infer(ctx, cfPrompt)
	if err != nil {
		cf = "If the final step had produced a different output, the task might have succeeded."
	}
	ref.Counterfactual = cf

	// 步骤 3 — 生成 Heuristic 并持久化
	heuristicContent := fmt.Sprintf("For %s tasks: %s. Avoid: %s", taskType, cf, cause)
	ref.GeneratedHeuristic = heuristicContent

	// 写入 HeuristicsMemory（启发式成功率从 0 开始，由后续任务 EWMA 更新）
	if re.heuristics != nil {
		hID := fmt.Sprintf("h_%s_%d", taskID, time.Now().UnixNano())
		if err := re.heuristics.Add(&Heuristic{
			ID:          hID,
			Content:     heuristicContent,
			TaskType:    taskType,
			SuccessRate: 0.5, // 冷启动中性值
			UseCount:    0,
			Keywords:    extractKeywords(taskType, cause),
		}); err != nil {
			_ = err // 写入失败不阻断主流程
		}
	}

	// 只有 Controllable/Logic 失败才写入 MEMF（Uncontrollable 排除）
	if !result.IsUncontrollable() && re.memf != nil {
		kwJSON, _ := json.Marshal(extractKeywords(taskType, cause))
		_ = kwJSON
		recordID := fmt.Sprintf("memf_%s_%d", taskID, time.Now().UnixNano())
		_ = re.memf.AddRecord(&FallacyRecord{
			ID:               recordID,
			TaskType:         taskType,
			FailureType:      string(result.FailureClass),
			Keywords:         extractKeywords(taskType, cause),
			Reflection:       cause + " | " + cf,
			OccurrenceCount:  1,
			NodeQualityScore: 0.5,
			CreatedAt:        time.Now().Unix(),
		})
		ref.MEMFRecordID = recordID
	}

	// 发布 HeuristicGeneratedPayload 给 self_improve.Engine 内环（闭环关键路径）。
	// 非阻塞发送：信道满时丢弃，后台尽力而为原则（M9 §6 降级策略）。
	if re.heuristicCh != nil {
		select {
		case re.heuristicCh <- protocol.HeuristicGeneratedPayload{
			TaskID:    taskID,
			TaskType:  taskType,
			Heuristic: heuristicContent,
			AvoidRule: cause, // 步骤1产出的失败原因作为 AvoidRule 种子
			CreatedAt: time.Now().Unix(),
		}:
		default:
			// 信道满，丢弃（后台任务尽力而为，不阻断反思主流程）
		}
	}

	return ref, nil
}

// replaySuccess 将成功纠偏轨迹提炼为 SurrealDB 技能记忆（AgentHER 核心）。
//
// 处理流程：
//  1. 调用 LLM，输入完整 trajectory（含失败步骤和最终成功步骤）
//  2. Prompt：提炼这次"犯错→成功"的关键洞察，输出 {"insight": "...", "tags": [...]}
//  3. 将 insight 写入 SurrealDB：
//     - FTSIndex(docID=taskID+"_her", text=insight)
//     - GraphRelate(taskType, "learned_from_failure", insight_id, weight=float64(replanCount))
//  4. 同时写入 reflection_memory 表：reflection_type='success_pattern'
//
// 注意：replaySuccess 异步执行（goroutine），不阻塞主反思流程。
func (re *ReflexionEngine) replaySuccess(
	ctx context.Context,
	taskID, taskType string,
	trajectory []Step,
	replanCount int,
) (*Reflection, error) {
	go func() {
		if re.llmInfer == nil {
			return
		}

		formattedTraj := formatTrajectory(trajectory)
		prompt := fmt.Sprintf(`Analyze this successful trajectory that required %d replans.
Extract the core pattern/insight of why it failed initially and how it eventually succeeded.
Output strictly JSON:
{
  "insight": "The specific learned strategy",
  "tags": ["tag1", "tag2"]
}
Trajectory:
%s`, replanCount, formattedTraj)

		insightJSON, err := re.llmInfer(context.Background(), prompt)
		if err != nil {
			return
		}

		var parsed struct {
			Insight string   `json:"insight"`
			Tags    []string `json:"tags"`
		}
		if err := json.Unmarshal([]byte(insightJSON), &parsed); err != nil {
			return
		}

		if parsed.Insight == "" {
			return
		}

		docID := "her_" + taskID
		if re.surreal != nil {
			_ = re.surreal.FTSIndex(docID, parsed.Insight+" "+fmt.Sprint(parsed.Tags))
			_ = re.surreal.GraphRelate("task_type:"+taskType, "learned_pattern", docID, float64(replanCount))
		}

		if re.db != nil {
			_, _ = re.db.ExecContext(context.Background(), `
				INSERT OR IGNORE INTO reflection_memory 
				  (task_id, reflection_type, content, created_at)
				VALUES (?, 'success_pattern', ?, ?)
			`, taskID, insightJSON, time.Now().Unix())
		}
	}()

	return &Reflection{
		TaskID:    taskID,
		Cause:     "success_after_replan",
		CreatedAt: time.Now().Unix(),
	}, nil
}

// ─── helpers ──────────────────────────────────────────────────────────────────

func formatTrajectory(traj []Step) string {
	var out string
	for _, s := range traj {
		status := "SUCCESS"
		if !s.Success {
			status = "FAILED"
		}
		out += fmt.Sprintf("Step %d: Action=%s, Result=%s, Status=%s\n", s.Index, s.Action, s.Result, status)
	}
	return out
}

// infer LLM 主路径 + 规则回退。
// DeepSeek ¥1/1M tokens 使反思分析的经济成本可忽略。
func (re *ReflexionEngine) infer(ctx context.Context, prompt string) (string, error) {
	if re.llmInfer != nil {
		return re.llmInfer(ctx, prompt)
	}
	// 离线/故障回退：返回空让调用方使用规则推断
	return "", nil
}

func buildCausePrompt(taskType string, trajectory []Step) string {
	lastStep := ""
	if len(trajectory) > 0 {
		s := trajectory[len(trajectory)-1]
		lastStep = fmt.Sprintf("Last action: %s, Result: %s", s.Action, s.Result)
	}
	return fmt.Sprintf(
		"Task type: %s\n%s\nAnalyze the root cause of failure in one concise sentence.",
		taskType, lastStep,
	)
}

func buildCounterfactualPrompt(taskType string, trajectory []Step, cause string) string {
	return fmt.Sprintf(
		"Task type: %s\nRoot cause: %s\nIn one sentence: what change in the approach would have led to success?",
		taskType, cause,
	)
}

// inferCauseFromTrajectory 从轨迹规则推断失败原因（LLM 不可用时的 fallback）。
func inferCauseFromTrajectory(trajectory []Step) string {
	if len(trajectory) == 0 {
		return "Unknown failure: no trajectory recorded."
	}
	last := trajectory[len(trajectory)-1]
	if !last.Success {
		return fmt.Sprintf("Failed at step %d: action '%s' produced '%s'", last.Index, last.Action, last.Result)
	}
	return "Task failed after all steps completed without clear error."
}

func extractKeywords(taskType, text string) []string {
	kw := []string{taskType}
	// 简单拆词（生产应使用 NLP 分词或 LLM 提取）
	words := []string{}
	current := ""
	for _, c := range text {
		if c == ' ' || c == '.' || c == ',' || c == ':' {
			if len(current) > 4 {
				words = append(words, current)
			}
			current = ""
		} else {
			current += string(c)
		}
	}
	if len(words) > 5 {
		words = words[:5]
	}
	return append(kw, words...)
}

// =============================================================================
// HeuristicsMemory.Add — SQLite 写入（Phase 2 补充）
// =============================================================================

// Add 将新启发式规则写入 SQLite。
// 若 ID 已存在则更新 success_rate 和 use_count（UPSERT）。
func (hm *HeuristicsMemory) Add(h *Heuristic) error {
	kwBytes, _ := json.Marshal(h.Keywords)
	_, err := hm.db.Exec(`
		INSERT INTO heuristics_memory (id, content, task_type, success_rate, use_count, keywords_json, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			success_rate = (success_rate * use_count + excluded.success_rate) / (use_count + 1),
			use_count = use_count + 1
	`, h.ID, h.Content, h.TaskType, h.SuccessRate, h.UseCount, string(kwBytes), time.Now().Unix())
	return err
}

// UpdateSuccessRate 更新启发式规则的成功率（EWMA α=0.1）。
func (hm *HeuristicsMemory) UpdateSuccessRate(id string, success bool) error {
	var currentRate float64
	err := hm.db.QueryRow("SELECT success_rate FROM heuristics_memory WHERE id = ?", id).Scan(&currentRate)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // 记录不存在，跳过
	}
	if err != nil {
		return err
	}
	var observation float64
	if success {
		observation = 1.0
	}
	// EWMA α=0.1
	newRate := 0.9*currentRate + 0.1*observation
	_, err = hm.db.Exec(
		"UPDATE heuristics_memory SET success_rate = ?, use_count = use_count + 1 WHERE id = ?",
		newRate, id,
	)
	return err
}
