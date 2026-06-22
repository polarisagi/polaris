package lam

import (
	"fmt"
	"math"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// ContinuousAction 用于 LAM (Large Action Model) 和 Diffusion Policy 的连续动作表示。
// 架构文档: docs/arch/07-Tool-Action-Layer-深度选型.md §7.2
// MVP 不实现 vision 解析路径，Computer Use 仅文本+坐标动作。vision 解析 Tier 1+ 研究分支。
type ContinuousAction struct {
	ActionType   string    // "tool_call" | "mouse_delta" | "key_sequence"
	ActionVector []float64 // 连续动作向量
	Horizon      int       // 预测时间步数
	Confidence   float64   // 0-1 置信度
}

// ActionDiscretizer 将连续动作向量转为离散工具调用。
type ActionDiscretizer struct {
	ActionMap map[string]ActionProjector
}

// ActionProjector 将连续向量投影到离散动作空间。
type ActionProjector interface {
	Project(vec []float64) (toolName string, args map[string]any, err error)
}

// Discretize 将 ContinuousAction 离散化为工具调用。
//
// 算法：枚举所有注册的 ActionProjector，计算输入向量与各 projector 质心的余弦相似度，
// 选取相似度最高的 projector 执行 Project()。
// 若置信度低于 0.3 或无可用 projector，返回错误。
//
// [接口预留][实现依赖 Tier-1+ 视觉输入 + 扩散策略模型，当前为最近邻启发式实现]
func (d *ActionDiscretizer) Discretize(action ContinuousAction) (toolName string, args map[string]any, err error) {
	if len(d.ActionMap) == 0 {
		return "", nil, apperr.New(apperr.CodeInternal, "action_discretizer: no projectors registered")
	}
	if action.Confidence < 0.3 {
		return "", nil, apperr.New(apperr.CodeInternal, fmt.Sprintf("action_discretizer: confidence %.2f below threshold 0.30", action.Confidence))
	}
	if len(action.ActionVector) == 0 {
		return "", nil, apperr.New(apperr.CodeInternal, "action_discretizer: empty action vector")
	}

	// 以字典序最小键为默认（保证确定性）
	bestKey := ""
	for k := range d.ActionMap {
		if bestKey == "" || k < bestKey {
			bestKey = k
		}
	}

	// 若只有一个 projector 直接委托
	if len(d.ActionMap) == 1 {
		return d.ActionMap[bestKey].Project(action.ActionVector)
	}

	// 多 projector：选余弦相似度最高的
	// 以 projector 名称编码为向量（哈希映射到维度）作为参考质心
	bestSim := math.Inf(-1)
	for k, proj := range d.ActionMap {
		centroid := keyToCentroid(k, len(action.ActionVector))
		sim := cosineSim(action.ActionVector, centroid)
		if sim > bestSim {
			bestSim = sim
			bestKey = k
			_ = proj
		}
	}
	return d.ActionMap[bestKey].Project(action.ActionVector)
}

// keyToCentroid 将字符串键映射为单位向量（用于 projector 选择中的质心近似）。
func keyToCentroid(key string, dim int) []float64 {
	if dim == 0 {
		return nil
	}
	vec := make([]float64, dim)
	for i, ch := range key {
		vec[i%dim] += float64(ch)
	}
	return normalizeVec(vec)
}

// cosineSim 计算两向量的余弦相似度，维度不一致时截断到较短维度。
func cosineSim(a, b []float64) float64 {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	var dot, normA, normB float64
	for i := range n {
		dot += a[i] * b[i]
		normA += a[i] * a[i]
		normB += b[i] * b[i]
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}

// normalizeVec 将向量归一化为单位向量（零向量保持不变）。
func normalizeVec(v []float64) []float64 {
	var norm float64
	for _, x := range v {
		norm += x * x
	}
	if norm == 0 {
		return v
	}
	norm = math.Sqrt(norm)
	out := make([]float64, len(v))
	for i, x := range v {
		out[i] = x / norm
	}
	return out
}
