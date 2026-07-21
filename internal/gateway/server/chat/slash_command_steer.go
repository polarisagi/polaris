package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"

	llmadapter "github.com/polarisagi/polaris/internal/llm/adapter"
)

// handleSteer M09-Self-Improvement-Engine.md §1.3 用户命令面：
// /steer list|import <label> <file>|set <label> <weight>|deactivate|delete <label>|calibrate-layer <task_type>
//
// steering/cvStore 均由 Server.SetSteering 注入（boot_server.go），未注入时
// （FeatureActivationSteer 未启用或 Tier<1）nil-safe 提示不可用，不影响其余
// 斜线命令。calibrate-layer 与"成功率<0.1 自动停用"两项本次未实现（见方法
// 内注释与 ADR-0054 后续记录），原因是二者分别需要额外的分层效果评估机制与
// 会话结果反馈信号，本仓库目前均不存在，不臆测语义强行接入（R1）。
func (r *SlashCommandRouter) handleSteer(ctx context.Context, args, sessionID string, w http.ResponseWriter, flusher http.Flusher) string {
	if r.steering == nil || r.cvStore == nil {
		msg := "激活引导未启用（需要 FeatureActivationSteer 门控开启，通常要求 Tier1+ 且已启用本地推理）"
		r.WriteSSEText(w, flusher, msg)
		return msg
	}

	fields := strings.Fields(args)
	if len(fields) == 0 {
		msg := "用法: /steer list | import <label> <file> | set <label> <weight> | deactivate | delete <label> | calibrate-layer <task_type>"
		r.WriteSSEText(w, flusher, msg)
		return msg
	}

	var resp string
	switch strings.ToLower(fields[0]) {
	case "list":
		resp = r.steerList()
	case "import":
		resp = r.steerImport(fields[1:])
	case "set":
		resp = r.steerSet(ctx, fields[1:], sessionID)
	case "deactivate":
		resp = r.steerDeactivate(ctx, sessionID)
	case "delete":
		resp = r.steerDelete(fields[1:])
	case "calibrate-layer":
		// M09 §1.3 命令面文档要求，但"校准"需要对同一 task_type 在多个 layer_id
		// 下运行效果评估（Eval Harness 尚无对应 case 类型）后择优——这是需要新
		// 设计的评估机制而非现有代码的桥接缺口，不在本次范围内臆测实现。
		resp = "calibrate-layer 暂未实现：需要新增按 layer_id 运行效果评估的机制（当前 Eval Harness 无此 case 类型），非一次接线可完成"
	default:
		resp = fmt.Sprintf("未知 /steer 子命令: %s（可用: list/import/set/deactivate/delete/calibrate-layer）", fields[0])
	}
	r.WriteSSEText(w, flusher, resp)
	return resp
}

func (r *SlashCommandRouter) steerList() string {
	labels := r.cvStore.List()
	if len(labels) == 0 {
		return "尚无已注册的控制向量。使用 /steer import <label> <file> 导入"
	}
	var sb strings.Builder
	sb.WriteString("**已注册控制向量**\n\n")
	for _, l := range labels {
		cv, ok := r.cvStore.Get(l)
		if !ok {
			continue
		}
		fmt.Fprintf(&sb, "- `%s`（layer=%d, dim=%d）\n", l, cv.Layer, len(cv.Vector))
	}
	return strings.TrimRight(sb.String(), "\n")
}

// steerVectorFile /steer import 读取的 JSON 文件格式：{"layer": 15, "vector": [...]}
// layer 可省略（默认 15）。
type steerVectorFile struct {
	Layer  int       `json:"layer"`
	Vector []float32 `json:"vector"`
}

func (r *SlashCommandRouter) steerImport(args []string) string {
	if len(args) < 2 {
		return "用法: /steer import <label> <file>（file 为 JSON: {\"layer\":15,\"vector\":[...]})"
	}
	label, path := args[0], args[1]
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("导入失败：读取文件出错: %v", err)
	}
	var spec steerVectorFile
	if err := json.Unmarshal(raw, &spec); err != nil {
		return fmt.Sprintf("导入失败：JSON 解析出错: %v", err)
	}
	if len(spec.Vector) == 0 {
		return "导入失败：vector 字段为空"
	}
	r.cvStore.Import(label, spec.Vector, spec.Layer)
	return fmt.Sprintf("已导入控制向量 `%s`（layer=%d, dim=%d）", label, orDefaultLayer(spec.Layer), len(spec.Vector))
}

func orDefaultLayer(layer int) int {
	if layer <= 0 {
		return 15
	}
	return layer
}

func (r *SlashCommandRouter) steerSet(ctx context.Context, args []string, sessionID string) string {
	if len(args) < 2 {
		return "用法: /steer set <label> <weight>"
	}
	label, weightStr := args[0], args[1]
	weight, err := strconv.ParseFloat(weightStr, 64)
	if err != nil {
		return fmt.Sprintf("weight 不是合法数字: %v", err)
	}
	cv, ok := r.cvStore.Get(label)
	if !ok {
		return fmt.Sprintf("未找到控制向量 `%s`，先用 /steer import 导入", label)
	}

	resp, err := r.steering.SteerActivations(ctx, &llmadapter.SteerRequest{
		Layer:     cv.Layer,
		Vector:    cv.Vector,
		Scale:     weight,
		SessionID: sessionID,
	})
	if err != nil {
		return fmt.Sprintf("激活引导应用失败: %v", err)
	}
	return fmt.Sprintf("已应用控制向量 `%s`（layer=%d, weight=%.2f, applied=%v）", label, resp.Layer, weight, resp.Applied)
}

func (r *SlashCommandRouter) steerDeactivate(ctx context.Context, sessionID string) string {
	if err := r.steering.ClearSteering(ctx, sessionID); err != nil {
		return fmt.Sprintf("清除激活引导失败: %v", err)
	}
	return "当前会话激活引导已清除"
}

func (r *SlashCommandRouter) steerDelete(args []string) string {
	if len(args) < 1 {
		return "用法: /steer delete <label>"
	}
	if r.cvStore.Delete(args[0]) {
		return fmt.Sprintf("已删除控制向量 `%s`", args[0])
	}
	return fmt.Sprintf("未找到控制向量 `%s`", args[0])
}
