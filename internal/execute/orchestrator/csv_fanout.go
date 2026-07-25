package orchestrator

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
	"github.com/polarisagi/polaris/pkg/types"
)

// CSVFanoutJob CSV batch fan-out 任务描述（ADR-0015 §2.5）。
// 每行 CSV → 一个 SubAgent Task → Blackboard 认领执行 → 结果聚合写回。
//
// 状态持久化：每行状态变更写 EventLog（event_type=csv_job_row_*），
// 遵循 HE-Rule-6 State-in-DB，禁止引入独立 SQLite。
type CSVFanoutJob struct {
	// CSVPath 输入 CSV 文件路径（第一行为 header）。
	CSVPath string
	// IDColumn 用于标识行的列名（空则用行号）。
	IDColumn string
	// Instruction 模板，支持 {column_name} 占位符替换。
	Instruction string
	// OutputCSVPath 结果输出路径（空则不写文件，只返回 FanoutResult）。
	OutputCSVPath string
	// MaxConcurrency 并发 SubAgent 上限（0 = 使用 Blackboard 默认）。
	MaxConcurrency int
	// MaxRuntimeSec 每个 worker 最大执行秒数（0 = 1800s）。
	MaxRuntimeSec int
	// EventLog 可选；nil 时跳过 EventLog 写入
	EventLog CSVFanoutEventLogger
}

// CSVFanoutEventLogger 类型别名：接口定义权属于消费方，已收敛至
// internal/protocol（见 protocol.CSVFanoutEventLogger）。此处保留别名以
// 避免改动所有调用点（D6/F4，原为 producer-side interface 违规）。
type CSVFanoutEventLogger = protocol.CSVFanoutEventLogger

// RowResult 单行 CSV 的执行结果。
type RowResult struct {
	ItemID  string
	Row     map[string]string // 原始行数据
	Status  string            // pending | running | done | error
	Result  string            // worker 报告的结果（JSON 字符串）
	Error   string
	StartAt time.Time
	DoneAt  time.Time
}

// FanoutResult CSV batch 整体结果。
type FanoutResult struct {
	JobID  string
	Total  int
	Done   int
	Errors int
	Rows   []RowResult
}

// csvFanoutBatchMultiplier 决定单批常驻内存的行数 = 并发度 × 该倍数。
// D6：内存占用与并发度挂钩，不与文件总行数挂钩，避免 Tier-0（2GB VPS）OOM。
const csvFanoutBatchMultiplier = 4

// RunCSVFanout 执行 CSV fan-out，将每行 CSV 作为独立任务发布到 Blackboard。
// 调用方负责提供 Blackboard 实例和 SubAgent 执行后端。
// 本函数：逐批流式读 CSV（不整文件 ReadAll）→ 构建 TaskEntry → PostBatch →
// 并发等待（ctx-aware 信号量）→ 聚合结果 → 若配置了输出路径则逐批追加写。
// D6（原 batch13 待分诊项 + GR-7-004 + GR-7-005）：修复 r.ReadAll() 全量加载
// OOM 风险与信号量获取未受 ctx.Done() 保护导致的潜在死锁挂起。
func RunCSVFanout(ctx context.Context, bb protocol.Blackboard, job CSVFanoutJob) (*FanoutResult, error) { //nolint:gocyclo
	if err := validateFanoutJob(&job); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "RunCSVFanout", err)
	}

	f, err := os.Open(job.CSVPath)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("csv_fanout: open %s", job.CSVPath), err)
	}
	defer f.Close()

	reader := csv.NewReader(f)
	headers, err := reader.Read()
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("csv_fanout: read header %s", job.CSVPath), err)
	}

	maxRuntime := job.MaxRuntimeSec
	if maxRuntime <= 0 {
		maxRuntime = 1800
	}
	concurrency := job.MaxConcurrency
	if concurrency <= 0 {
		concurrency = 6
	}
	batchSize := concurrency * csvFanoutBatchMultiplier

	jobID := fmt.Sprintf("csv-job-%d", time.Now().UnixNano())
	deadline := time.Now().Add(time.Duration(maxRuntime) * time.Second)
	waitCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	sem := make(chan struct{}, concurrency)

	var outWriter *csv.Writer
	if job.OutputCSVPath != "" {
		outFile, ferr := os.Create(job.OutputCSVPath)
		if ferr != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "csv_fanout: create output", ferr)
		}
		defer outFile.Close()
		outWriter = csv.NewWriter(outFile)
		outHeaders := append(append([]string{}, headers...), "job_id_row", "status", "result", "error", "duration_ms")
		if werr := outWriter.Write(outHeaders); werr != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "csv_fanout: write output header", werr)
		}
	}

	var allResults []RowResult
	rowIdx := 0
	batchNum := 0
	canceled := false

	for {
		batch, readErr := readCSVBatch(reader, headers, batchSize)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return nil, apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("csv_fanout: read %s", job.CSVPath), readErr)
		}
		if len(batch) == 0 {
			break
		}
		batchNum++

		entries := make([]*types.TaskEntry, len(batch))
		batchResults := make([]RowResult, len(batch))
		for i, row := range batch {
			idx := rowIdx + i
			itemID := itemIDForRow(row, headers, job.IDColumn, idx)
			instruction := expandTemplate(job.Instruction, row)
			entries[i] = &types.TaskEntry{
				ID:       fmt.Sprintf("%s-row-%d", jobID, idx),
				Type:     "csv_fanout_row",
				Priority: 5,
				Intent:   []byte(instruction),
				// GD-14-001：同一 CSV fanout job 下的所有行共享同一记忆命名空间
				// （namespace = jobID），使处理不同行的 Worker Agent 能检索到彼此
				// 写入的记忆片段（如跨行发现的共性规律）；不同 job 之间天然隔离。
				Namespace: jobID,
			}
			batchResults[i] = RowResult{ItemID: itemID, Row: row, Status: "pending"}
		}

		if err := bb.PostBatch(ctx, entries); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("csv_fanout: post batch: %v", err), err)
		}

		if job.EventLog != nil {
			if errEvent := job.EventLog.Append(ctx, types.Event{
				ID:      fmt.Sprintf("%s_posted_batch_%d", jobID, batchNum),
				Type:    types.EventType("csv_job_row_posted"),
				TaskID:  jobID,
				Payload: []byte(fmt.Sprintf(`{"job_id":%q,"batch":%d,"batch_size":%d}`, jobID, batchNum, len(batch))),
			}); errEvent != nil {
				slog.Warn("csv_fanout: append job_posted event failed", "jobID", jobID, "err", errEvent)
			}
		}

		var wg sync.WaitGroup
		var mu sync.Mutex
		for i, entry := range entries {
			wg.Add(1)
			select {
			case sem <- struct{}{}:
			case <-waitCtx.Done():
				// ctx 已取消/超时：不再派发新 worker，直接标记该行失败并继续
				// 收尾（避免在满载 channel 上永久阻塞，GR-7-005）。
				wg.Done()
				canceled = true
				mu.Lock()
				batchResults[i].Status = "error"
				batchResults[i].Error = waitCtx.Err().Error()
				mu.Unlock()
				continue
			}
			idx := i
			e := entry
			concurrent.SafeGo(waitCtx, "swarm.csv_fanout_worker", func(ctx context.Context) {
				defer wg.Done()
				defer func() { <-sem }()

				start := time.Now()
				mu.Lock()
				batchResults[idx].Status = "running"
				batchResults[idx].StartAt = start
				mu.Unlock()

				result, taskErr := waitForTask(ctx, bb, e.ID)

				mu.Lock()
				defer mu.Unlock()
				batchResults[idx].DoneAt = time.Now()
				if taskErr != nil {
					batchResults[idx].Status = "error"
					batchResults[idx].Error = taskErr.Error()
				} else {
					batchResults[idx].Status = "done"
					batchResults[idx].Result = result
				}

				if job.EventLog != nil {
					evType := "csv_job_row_done"
					if taskErr != nil {
						evType = "csv_job_row_error"
					}
					if errEvent := job.EventLog.Append(ctx, types.Event{
						ID:      e.ID + "_result",
						Type:    types.EventType(evType),
						TaskID:  jobID,
						Payload: []byte(fmt.Sprintf(`{"row_id":%q,"status":%q}`, e.ID, batchResults[idx].Status)),
					}); errEvent != nil {
						slog.Warn("csv_fanout: append job_row event failed", "rowID", e.ID, "err", errEvent)
					}
				}
			})
		}
		wg.Wait()

		if outWriter != nil {
			if werr := appendResultRows(outWriter, headers, batchResults); werr != nil {
				return nil, apperr.Wrap(apperr.CodeInternal, "csv_fanout: write output rows", werr)
			}
		}
		allResults = append(allResults, batchResults...)
		rowIdx += len(batch)

		if errors.Is(readErr, io.EOF) || canceled {
			break
		}
	}

	fanout := &FanoutResult{
		JobID: jobID,
		Total: rowIdx,
		Rows:  allResults,
	}
	for _, r := range allResults {
		switch r.Status {
		case "done":
			fanout.Done++
		case "error":
			fanout.Errors++
		}
	}
	return fanout, nil
}

// waitForTask 轮询 Blackboard 等待 Task 达到终态（done/failed）。
func waitForTask(ctx context.Context, bb protocol.Blackboard, taskID string) (string, error) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return "", apperr.New(apperr.CodeInternal, fmt.Sprintf("timeout waiting for task %s", taskID))
		case <-ticker.C:
			snap, err := bb.PeekTask(ctx, taskID)
			if err != nil {
				return "", apperr.Wrap(apperr.CodeInternal, "waitForTask", err)
			}
			if snap == nil {
				continue
			}
			switch snap.Status {
			case types.TaskDone:
				return string(snap.Result), nil
			case types.TaskFailed:
				return "", apperr.New(apperr.CodeInternal, fmt.Sprintf("task %s failed", taskID))
			}
		}
	}
}

// readCSVBatch 从已打开的 csv.Reader 逐行流式读取最多 n 行（不做 ReadAll 全量
// 加载），返回本批次的行数据（每行为 map[列名→值]）。读到 io.EOF 时返回已读到
// 的行 + io.EOF，调用方据此判断是否为最后一批。
func readCSVBatch(r *csv.Reader, headers []string, n int) ([]map[string]string, error) {
	rows := make([]map[string]string, 0, n)
	for i := 0; i < n; i++ {
		record, err := r.Read()
		if errors.Is(err, io.EOF) {
			return rows, io.EOF
		}
		if err != nil {
			return rows, apperr.Wrap(apperr.CodeInternal, "readCSVBatch", err)
		}
		row := make(map[string]string, len(headers))
		for j, h := range headers {
			if j < len(record) {
				row[h] = record[j]
			}
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// expandTemplate 替换 {column_name} 占位符为行数据值。
func expandTemplate(template string, row map[string]string) string {
	result := template
	for k, v := range row {
		result = strings.ReplaceAll(result, "{"+k+"}", v)
	}
	return result
}

func itemIDForRow(row map[string]string, headers []string, idCol string, idx int) string {
	if idCol != "" {
		if v, ok := row[idCol]; ok && v != "" {
			return v
		}
	}
	if len(headers) > 0 {
		if v, ok := row[headers[0]]; ok && v != "" {
			return v
		}
	}
	return fmt.Sprintf("row-%d", idx)
}

// appendResultRows 将一个批次的行结果追加写入已打开的 csv.Writer 并 Flush。
// 与旧版 writeResultCSV（一次性等到全部行处理完再整体写出）不同，本函数按批次
// 增量落盘，避免结果集随总行数线性增长而常驻内存（D6）。
func appendResultRows(w *csv.Writer, headers []string, results []RowResult) error {
	for _, r := range results {
		durMs := r.DoneAt.Sub(r.StartAt).Milliseconds()
		record := make([]string, 0, len(headers)+5)
		for _, h := range headers {
			record = append(record, r.Row[h])
		}
		record = append(record,
			r.ItemID,
			r.Status,
			r.Result,
			r.Error,
			fmt.Sprintf("%d", durMs),
		)
		if err := w.Write(record); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "appendResultRows", err)
		}
	}
	w.Flush()
	if ferr := w.Error(); ferr != nil {
		return apperr.Wrap(apperr.CodeInternal, "appendResultRows: flush", ferr)
	}
	return nil
}

func validateFanoutJob(job *CSVFanoutJob) error {
	if job.CSVPath == "" {
		return apperr.New(apperr.CodeInternal, "csv_fanout: CSVPath is required")
	}
	if job.Instruction == "" {
		return apperr.New(apperr.CodeInternal, "csv_fanout: Instruction is required")
	}
	return nil
}
