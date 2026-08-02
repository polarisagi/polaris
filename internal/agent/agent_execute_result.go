package agent

// 2026-08-02 从 agent_execute_util.go 拆分（Test_inv_FileLineLimit R7 400 行上限
// 存量债务，见 local_playground/upgrade/99-new-findings.md 阶段03 R-07 发现）：
// 本文件收敛 DAG 执行结果的聚合/截断/污点计算，纯搬运无行为变更。

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// mergeResumedExecuteResult 合并崩溃前快照的聚合结果（prior）与本次续跑新
// 产出的结果（fresh），用于 GD-13-009 崩溃恢复续跑场景。两者都可能是任意
// 字节内容（单节点场景 aggregateDAGResults 直接返回工具原始输出，未必是
// 合法 JSON），故不能用 json.RawMessage 直接拼接——改用字符串字段承载，
// json.Marshal 会对内容做安全转义，保证输出恒为合法 JSON。
func mergeResumedExecuteResult(prior, fresh []byte) []byte {
	envelope := struct {
		ResumedPriorResults string `json:"resumed_prior_results"`
		PostResumeResults   string `json:"post_resume_results"`
	}{
		ResumedPriorResults: string(prior),
		PostResumeResults:   string(fresh),
	}
	merged, err := json.Marshal(envelope)
	if err != nil {
		// 理论上字符串字段的 json.Marshal 不会失败；fail-safe 退回仅使用新结果，
		// 不让一个不可能发生的序列化错误阻断整条 DAG 执行完成路径。
		slog.Warn("agent: mergeResumedExecuteResult marshal failed, falling back to fresh results only", "err", err)
		return fresh
	}
	return merged
}

// aggregateDAGResults 将多节点执行结果聚合为统一 JSON 格式。
// 单节点直接返回 output；多节点序列化为 {"results":[{id,output},...]}.
func aggregateDAGResults(results []protocol.NodeResult) []byte {
	if len(results) == 0 {
		return []byte("{}")
	}
	if len(results) == 1 {
		if results[0].Err != nil {
			return []byte(`{"error":"` + results[0].Err.Error() + `"}`)
		}
		if len(results[0].Output) == 0 {
			if len(results[0].ImageParts) > 0 {
				return []byte("[Success (Image Attached)]")
			}
			return []byte("[Success (Empty Output)]")
		}
		return results[0].Output
	}

	// 多节点：构建聚合结构
	buf := make([]byte, 0, 256+len(results)*64)
	buf = append(buf, `{"results":[`...)
	for i, r := range results {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = append(buf, `{"id":"`...)
		buf = append(buf, r.NodeID...)
		buf = append(buf, `","output":`...)
		if r.Err != nil {
			buf = append(buf, `{"error":"`...)
			buf = append(buf, r.Err.Error()...)
			buf = append(buf, `"}`...)
		} else if len(r.Output) > 0 {
			buf = append(buf, r.Output...)
		} else if len(r.ImageParts) > 0 {
			buf = append(buf, `"[Success (Image Attached)]"`...)
		} else {
			buf = append(buf, `"[Success (Empty Output)]"`...)
		}
		buf = append(buf, '}')
	}
	buf = append(buf, "]}"...)
	return buf
}

// maxExecResultBytes 注入 LLM 的工具执行结果最大字节数（≈ 2000 token × 4 bytes/token）。
const maxExecResultBytes = 8000

// truncateExecResult 截断过长的执行结果，超限部分落盘并返回 log_ref 占位符。
// 落盘路径：~/.polarisagi/polaris/logs/exec_results/<logID>.txt
// LLM 收到：原文（≤8KB）或 <log_ref id="<logID>" bytes="<N>" /> 提示符（>8KB）
func truncateExecResult(sessionID string, raw []byte) []byte {
	if len(raw) <= maxExecResultBytes {
		return raw
	}

	logID := fmt.Sprintf("%s-%d", sessionID, time.Now().UnixNano())
	logDir := filepath.Join(os.ExpandEnv("$HOME"), ".polarisagi", "polaris", "logs", "exec_results")
	// 创建目录（best-effort，失败不阻断）
	if err := os.MkdirAll(logDir, 0700); err == nil {
		logPath := filepath.Join(logDir, logID+".txt")
		_ = os.WriteFile(logPath, raw, 0600)
	}

	// 截取前 512 字节作为内联预览，其余引用 log_ref
	preview := raw[:512]
	ref := fmt.Sprintf(
		"<log_ref id=%q bytes=%d />\n[Preview]\n%s\n[...truncated, see log]",
		logID, len(raw), preview,
	)
	return []byte(ref)
}

// maxNodeTaintLevel 计算 protocol.DAGPlan 中所有节点的最高污点等级。
// 实现 ADR-0007 PropagateTaint 语义：output = max(inputs)，只升不降。
// plan 为 nil 或无节点时返回 TaintNone（validateTaintGate 自动跳过）。
func maxNodeTaintLevel(plan *protocol.DAGPlan) types.TaintLevel {
	if plan == nil {
		return types.TaintNone
	}
	var max types.TaintLevel
	for _, node := range plan.Nodes {
		if node.TaintLevel > max {
			max = node.TaintLevel
		}
	}
	return max
}
