// Package checkpoint 提供影子 git 写前快照能力（ADR-0115 WP1）。
package checkpoint

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/polarisagi/polaris/pkg/types"
)

// Trigger 负责在满足条件时触发写前快照（ADR-0115 WP1）。
type Trigger struct {
	store            *Store
	workspaceDir     string
	throttleMax      int
	turnSnapshots    atomic.Int32
	sessionFirstDone atomic.Bool
}

// NewTrigger 创建快照触发器。
func NewTrigger(store *Store, workspaceDir string, throttleMax int) *Trigger {
	return &Trigger{
		store:        store,
		workspaceDir: workspaceDir,
		throttleMax:  throttleMax,
	}
}

// ResetTurn 在新回合开始时重置当回合快照节流计数。
func (t *Trigger) ResetTurn() {
	t.turnSnapshots.Store(0)
}

// TurnSnapshotCount 返回当回合已执行的快照次数。
func (t *Trigger) TurnSnapshotCount() int {
	return int(t.turnSnapshots.Load())
}

// MaybeSnapshot 判断是否需要打快照并在需要时执行快照。
// 触发规则（ADR-0115 WP1）：
//  1. 会话首个写操作前打一次快照；
//  2. 删除操作；
//  3. taint=High 的写类调用；
//  4. 写类且在工作区外。
//
// 打快照受 throttleMax 节流（同一回合最多打 N 次，默认 20）。
func (t *Trigger) MaybeSnapshot(ctx context.Context, toolName string, toolCap types.CapabilityLevel, sideEffects []types.SideEffect, input []byte, taintLevel types.TaintLevel) error {
	if t == nil || t.store == nil {
		return nil
	}

	isWrite := isWriteOperation(toolName, toolCap, sideEffects)
	if !isWrite {
		return nil
	}

	shouldSnapshot := false
	reason := ""

	// 条件 1：会话首个写操作
	if !t.sessionFirstDone.Swap(true) {
		shouldSnapshot = true
		reason = "session_first_write"
	}

	// 条件 2：删除操作
	if !shouldSnapshot && isDeleteOperation(toolName, input) {
		shouldSnapshot = true
		reason = "delete_operation"
	}

	// 条件 3：taint >= High 的写操作
	if !shouldSnapshot && taintLevel >= types.TaintHigh {
		shouldSnapshot = true
		reason = "high_taint_write"
	}

	// 条件 4：写类且在工作区外
	if !shouldSnapshot && isOutsideWorkspace(t.workspaceDir, input) {
		shouldSnapshot = true
		reason = "outside_workspace_write"
	}

	if !shouldSnapshot {
		return nil
	}

	// 节流检查
	n := t.turnSnapshots.Add(1)
	if t.throttleMax > 0 && int(n) > t.throttleMax {
		slog.Warn("checkpoint: throttle limit reached for turn, skipping snapshot",
			"tool", toolName, "turn_snapshots", n, "throttle_max", t.throttleMax)
		return nil
	}

	msg := fmt.Sprintf("pre-%s: %s (trigger: %s)", toolName, summarizeInput(input), reason)
	hash, err := t.store.Create(ctx, msg)
	if err != nil {
		slog.Error("checkpoint: failed to create pre-execution snapshot", "tool", toolName, "err", err)
		return nil
	}
	slog.Info("checkpoint: pre-execution snapshot taken", "tool", toolName, "hash", hash, "reason", reason)
	return nil
}

func isWriteOperation(toolName string, cap types.CapabilityLevel, sideEffects []types.SideEffect) bool {
	switch toolName {
	case "write_file", "str_replace_editor", "multi_edit", "notebook_edit", "bash", "run_command":
		return true
	}
	if cap >= types.CapWriteLocal {
		return true
	}
	for _, se := range sideEffects {
		if se == types.SideFileWrite || se == types.SideProcessSpawn {
			return true
		}
	}
	return false
}

func isDeleteOperation(toolName string, input []byte) bool {
	lower := strings.ToLower(toolName)
	if strings.Contains(lower, "delete") || strings.Contains(lower, "remove") || lower == "rm" {
		return true
	}
	var obj map[string]any
	if err := json.Unmarshal(input, &obj); err == nil {
		if cmd, ok := obj["command"].(string); ok {
			cmd = strings.TrimSpace(cmd)
			if strings.HasPrefix(cmd, "rm ") || strings.HasPrefix(cmd, "rmdir ") || strings.Contains(cmd, " rm -") {
				return true
			}
		}
	}
	return false
}

func isOutsideWorkspace(workspaceDir string, input []byte) bool {
	if workspaceDir == "" {
		return false
	}
	var obj map[string]any
	if err := json.Unmarshal(input, &obj); err != nil {
		return false
	}
	targetPath := ""
	if p, ok := obj["path"].(string); ok && p != "" {
		targetPath = p
	} else if p, ok := obj["working_dir"].(string); ok && p != "" {
		targetPath = p
	}
	if targetPath == "" {
		return false
	}
	abs, err := filepath.Abs(targetPath)
	if err != nil {
		return false
	}
	wsAbs, err := filepath.Abs(workspaceDir)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(wsAbs, abs)
	if err != nil {
		return false
	}
	// Rel 为 ".." 或以 "../" 开头表示在工作区外
	return strings.HasPrefix(rel, "..")
}

func summarizeInput(input []byte) string {
	s := strings.TrimSpace(string(input))
	if len(s) > 60 {
		return s[:60] + "..."
	}
	return s
}
