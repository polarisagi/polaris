package swarm

import (
	"context"
	"encoding/csv"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	perrors "github.com/polarisagi/polaris/internal/errors"
	"github.com/polarisagi/polaris/internal/protocol"
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

type CSVFanoutEventLogger interface {
	Append(ctx context.Context, ev protocol.Event) error
}

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

// RunCSVFanout 执行 CSV fan-out，将每行 CSV 作为独立任务发布到 Blackboard。
// 调用方负责提供 Blackboard 实例和 SubAgent 执行后端。
// 本函数：读 CSV → 构建 TaskEntry → PostBatch → 并发等待 → 聚合结果。
func RunCSVFanout(ctx context.Context, bb protocol.Blackboard, job CSVFanoutJob) (*FanoutResult, error) { //nolint:gocyclo
	if err := validateFanoutJob(&job); err != nil {
		return nil, err
	}

	headers, rows, err := readCSV(job.CSVPath)
	if err != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, fmt.Sprintf("csv_fanout: read %s: %v", job.CSVPath, err), err)
	}

	maxRuntime := job.MaxRuntimeSec
	if maxRuntime <= 0 {
		maxRuntime = 1800
	}

	jobID := fmt.Sprintf("csv-job-%d", time.Now().UnixNano())
	results := make([]RowResult, len(rows))
	entries := make([]*protocol.TaskEntry, 0, len(rows))

	for i, row := range rows {
		itemID := itemIDForRow(row, headers, job.IDColumn, i)
		instruction := expandTemplate(job.Instruction, row)

		entry := &protocol.TaskEntry{
			ID:       fmt.Sprintf("%s-row-%d", jobID, i),
			Type:     "csv_fanout_row",
			Priority: 5,
			Intent:   []byte(instruction),
		}
		entries = append(entries, entry)
		results[i] = RowResult{
			ItemID: itemID,
			Row:    row,
			Status: "pending",
		}
	}

	if err := bb.PostBatch(ctx, entries); err != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, fmt.Sprintf("csv_fanout: post batch: %v", err), err)
	}

	// 写 EventLog: csv_job_row_posted（一条聚合，避免单行 N 条爆日志）
	if job.EventLog != nil {
		_ = job.EventLog.Append(ctx, protocol.Event{
			ID:      jobID + "_posted",
			Type:    protocol.EventType("csv_job_row_posted"),
			TaskID:  jobID,
			Payload: []byte(fmt.Sprintf(`{"job_id":%q,"total":%d}`, jobID, len(rows))),
		})
	}

	// 并发等待所有 Task 完成（简化版：轮询 Blackboard 状态）
	concurrency := job.MaxConcurrency
	if concurrency <= 0 {
		concurrency = 6
	}
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex

	deadline := time.Now().Add(time.Duration(maxRuntime) * time.Second)
	waitCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	for i, entry := range entries {
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int, e *protocol.TaskEntry) {
			defer wg.Done()
			defer func() { <-sem }()

			start := time.Now()
			mu.Lock()
			results[idx].Status = "running"
			results[idx].StartAt = start
			mu.Unlock()

			result, taskErr := waitForTask(waitCtx, bb, e.ID)

			mu.Lock()
			defer mu.Unlock()
			results[idx].DoneAt = time.Now()
			if taskErr != nil {
				results[idx].Status = "error"
				results[idx].Error = taskErr.Error()
			} else {
				results[idx].Status = "done"
				results[idx].Result = result
			}

			if job.EventLog != nil {
				evType := "csv_job_row_done"
				if taskErr != nil {
					evType = "csv_job_row_error"
				}
				_ = job.EventLog.Append(waitCtx, protocol.Event{
					ID:      e.ID + "_result",
					Type:    protocol.EventType(evType),
					TaskID:  jobID,
					Payload: []byte(fmt.Sprintf(`{"row_id":%q,"status":%q}`, e.ID, results[idx].Status)),
				})
			}
		}(i, entry)
	}
	wg.Wait()

	fanout := &FanoutResult{
		JobID: jobID,
		Total: len(rows),
		Rows:  results,
	}
	for _, r := range results {
		switch r.Status {
		case "done":
			fanout.Done++
		case "error":
			fanout.Errors++
		}
	}

	if job.OutputCSVPath != "" {
		if err := writeResultCSV(job.OutputCSVPath, headers, results); err != nil {
			return fanout, perrors.Wrap(perrors.CodeInternal, fmt.Sprintf("csv_fanout: write output: %v", err), err)
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
			return "", perrors.New(perrors.CodeInternal, fmt.Sprintf("timeout waiting for task %s", taskID))
		case <-ticker.C:
			snap, err := bb.PeekTask(ctx, taskID)
			if err != nil {
				return "", err
			}
			if snap == nil {
				continue
			}
			switch snap.Status {
			case protocol.TaskDone:
				return string(snap.Result), nil
			case protocol.TaskFailed:
				return "", perrors.New(perrors.CodeInternal, fmt.Sprintf("task %s failed", taskID))
			}
		}
	}
}

// readCSV 读取 CSV 文件，返回 headers 和行数据（每行为 map[列名→值]）。
func readCSV(path string) (headers []string, rows []map[string]string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()

	r := csv.NewReader(f)
	records, err := r.ReadAll()
	if err != nil {
		return nil, nil, err
	}
	if len(records) < 2 {
		return nil, nil, perrors.New(perrors.CodeInternal, "CSV must have header row and at least one data row")
	}

	headers = records[0]
	for _, record := range records[1:] {
		row := make(map[string]string, len(headers))
		for i, h := range headers {
			if i < len(record) {
				row[h] = record[i]
			}
		}
		rows = append(rows, row)
	}
	return headers, rows, nil
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

func writeResultCSV(path string, headers []string, results []RowResult) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	w := csv.NewWriter(f)
	outHeaders := append(headers, "job_id_row", "status", "result", "error", "duration_ms")
	if err := w.Write(outHeaders); err != nil {
		return err
	}

	for _, r := range results {
		durMs := r.DoneAt.Sub(r.StartAt).Milliseconds()
		record := make([]string, 0, len(outHeaders))
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
			return err
		}
	}
	w.Flush()
	return w.Error()
}

func validateFanoutJob(job *CSVFanoutJob) error {
	if job.CSVPath == "" {
		return perrors.New(perrors.CodeInternal, "csv_fanout: CSVPath is required")
	}
	if job.Instruction == "" {
		return perrors.New(perrors.CodeInternal, "csv_fanout: Instruction is required")
	}
	return nil
}
