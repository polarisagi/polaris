package embedonnx

import (
	"context"
	"log/slog"
	"math"
	"sort"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// PrefKeyTierBench 是保存基准测定结果的偏好设置键名。
const PrefKeyTierBench = "embed.tier_bench"

// 向量化基准档位常量。
const (
	TierBalanced = "balanced" // 平衡档（EmbeddingGemma-300M ONNX 512维）
	TierLight    = "light"    // 轻量档（bge-small-zh-v1.5 ONNX 512维）
	TierNone     = "none"     // 降级档（不挂嵌入器，走 FTS）
)

// TierBenchResult 记录基准测试判定结果。
type TierBenchResult struct {
	Tier         string    `json:"tier"`
	ModelVersion string    `json:"model_version"`
	P95Ms        float64   `json:"p95_ms"`
	CPUBefore    float64   `json:"cpu_before"`
	CPUAfter     float64   `json:"cpu_after"`
	Contended    bool      `json:"contended"`
	RetryOnStart bool      `json:"retry_on_start"`
	MeasuredAt   time.Time `json:"measured_at"`
}

// BenchSampleTexts 返回 64 条固定中文短文本样本（10-60 字，中英混合）。
// 用于基准测试单条 p95 推理耗时测定。
func BenchSampleTexts() []string {
	return []string{
		"人工智能助手正在协助开发者编写高质量的单元测试代码。",
		"Polaris 守护进程提供了轻量级跨平台本地部署能力。",
		"系统日志显示后台重嵌任务在空闲窗口内平稳执行。",
		"SurrealDB 提供了高性能的 HNSW 向量索引与图遍历支持。",
		"内存阶梯自动升档机制已彻底废除，改为基于基准测定选档。",
		"使用 purego 动态链接可以在零 CGO 约束下调用 ORT 库。",
		"本地推理默认关闭，保障系统在低配硬件环境下的稳定性。",
		"EmbeddingGemma 采用 Matryoshka 截断至 512 维并重新归一化。",
		"bge-small-zh 模型利用 CLS 向量作为整句语义表征。",
		"多轮交互对话过程中，会话历史会自动压缩归档。",
		"如何配置 DeepSeek V4 作为默认的推理提供商？",
		"知识库检索支持混合检索与倒排全文检索互为补充。",
		"Cedar 策略引擎实现了精准的细粒度权限判定与门控拦截。",
		"任务调度器支持定时任务与 HITL 审批事件流驱动。",
		"请帮我分析这段 Go 代码的并发安全问题与内存泄漏风险。",
		"通过 Prometheus 埋点可以精确监控系统的 Token 消耗速率。",
		"使用 Docker 容器化部署可以将服务隔离在独立的网络空间。",
		"当前硬件环境未检测到可用的独立显卡，自动采用 CPU 推理。",
		"客户端与服务端通过安全的 HTTP 与 SSE 协议进行双向通信。",
		"请查询上周关于系统架构重构的所有评审会议纪要。",
		"向量维度由 2560 迁移至 512 维时需要重建 HNSW 索引。",
		"插件系统支持动态注册工具能力并按策略进行沙箱隔离。",
		"在纯 CPU 机器上进行大模型推理可能会导致系统负载过高。",
		"WordPiece 分词器能够高效切分中文单字与英文前缀子词。",
		"SentencePiece 使用纯 Go 实现，无须引入外部 C/C++ 依赖。",
		"系统启动后延迟 60 秒启动后台向量化基准测定。",
		"空闲超时达到 10 分钟后，ONNX 会话将自动从内存卸载。",
		"若 CPU 持续繁忙，基准测试将标记为竞争状态并在下次启动重试。",
		"开源自托管 AI Agent 架构设计需要兼顾性能与可观测性。",
		"本地大语言模型支持通过 Ollama 统一管理服务生命周期。",
		"如何在 macOS 上使用 launchd 配置守护进程开机自动启动？",
		"对搜索结果进行重排可以显著提升 Top-K 的命中准确率。",
		"情景记忆与语义记忆协同工作，构建长周期认知网络。",
		"请帮我生成一份关于数据迁移方案的详细技术架构文档。",
		"当网络连接不稳定时，下载器会自动切换到备用镜像节点。",
		"静态代码分析工具检查发现当前文件中存在未捕获的错误分支。",
		"用户在前端设置页面点击了重新评估嵌入器性能的按钮。",
		"为保护敏感凭据安全，环境变量中的 API Key 不会被明文输出。",
		"批处理通道采用独立队列设计，避免后台请求阻塞交互查询。",
		"请总结这篇技术论文的核心贡献与实验对比数据分析。",
		"通过指数退避机制可以有效防止后台失败重试刷爆系统资源。",
		"由于当前可用内存不足 600MB，系统强制降级为轻量档模型。",
		"微调模型权重需要准备标注良好的领域特定问答数据集。",
		"利用动态规划算法求解该问题可以获得最优的时间复杂度。",
		"HTTP 服务端在收到关机信号后会优雅等待在途连接处理完毕。",
		"请为该 REST API 接口补充详细的 OpenAPI 规范说明。",
		"多智能体协同框架支持通过黑板模式共享任务上下文状态。",
		"系统探针已完成对 CPU 物理核心数与负载情况的探测采样。",
		"当嵌入器未挂载时，检索链路平滑回退至 SQLite FTS 全文搜索。",
		"测试套件覆盖了表驱动用例与边界异常条件的断言验证。",
		"向量余弦相似度计算结果与 Python 参考实现完全对齐。",
		"如果单条推理延迟过高，模型将无法满足实时交互的响应需求。",
		"请检查该数据库表的索引定义是否与最新业务查询模式匹配。",
		"通过分布式锁可以保证高并发场景下数据变更的一致性。",
		"代码重构之后，主入口文件的圈复杂度得到了显著降低。",
		"系统支持中文和英文双语界面无缝切换与国际化文本映射。",
		"在极简模式下运行可以最大限度节省边缘设备的电力消耗。",
		"所有输入数据在进入大语言模型前均经过敏感词安全过滤。",
		"编译期常量钉死依赖库版本，避免动态链接符号失配风险。",
		"模型权重文件的 SHA256 校验和与发布清单完全保持一致。",
		"请协助排查容器启动时报连接被拒绝的具体网络配置根因。",
		"自动化测试流水线将在合并代码前运行完整的静态扫描。",
		"使用内存缓存可以避免短时间内对相同查询文本重复向量化。",
		"通过本次基准评估，系统成功选出了与当前硬件最匹配的模型。",
	}
}

// WaitForCPUIdle 等待 CPU 空闲窗口（复刻 ADR-0108 D2 语义）。
// 每 2s 采样一次，连续 3 次 CPU < 50% 视为进入空闲窗口。
// 若超过 maxWait 仍未达到，返回 false（表示 Contended 状态）。
func WaitForCPUIdle(ctx context.Context, cpuUsage func() float64, maxWait time.Duration) bool {
	if maxWait <= 0 {
		maxWait = 10 * time.Minute
	}
	deadline := time.Now().Add(maxWait)
	consecutiveLow := 0

	for {
		if time.Now().After(deadline) {
			slog.Warn("embedonnx: CPU idle window wait timed out (contended)", "max_wait", maxWait)
			return false
		}

		select {
		case <-ctx.Done():
			return false
		default:
		}

		usage := cpuUsage()
		if usage < 50.0 {
			consecutiveLow++
			if consecutiveLow >= 3 {
				return true
			}
		} else {
			consecutiveLow = 0
		}

		select {
		case <-ctx.Done():
			return false
		case <-time.After(2 * time.Second):
		}
	}
}

// DecideTier 依据内存状态、测得的 p95 延迟及竞争状态判定档位。
// 规则严格按照 PROMPT.md §3 P2-7 / ADR-0109：
// 1. 内存下限（总内存 < 1.5GB 或可用 < 600MB）→ 直接轻量档；
// 2. 平衡档 p95 <= 80ms → 平衡档；
// 3. 平衡档 > 80ms 且轻量档 <= 30ms 且 !contended → 轻量档；
// 4. 平衡档 > 80ms 且 contended → 轻量档，不落定论（retry_on_start=true）；
// 5. 平衡档 > 80ms 且轻量档 > 30ms 且 !contended → 不挂嵌入器（FTS）。
func DecideTier(memTotalMB, memAvailMB uint64, gemmaP95Ms, bgeP95Ms float64, contended bool) TierBenchResult {
	res := TierBenchResult{
		MeasuredAt: time.Now(),
		Contended:  contended,
	}

	// 规则 1：内存下限
	if (memTotalMB > 0 && memTotalMB < 1536) || (memAvailMB > 0 && memAvailMB < 600) {
		res.Tier = TierLight
		res.ModelVersion = ModelVersionBGE
		res.P95Ms = bgeP95Ms
		return res
	}

	// 规则 2：平衡档达标 (<= 80ms)
	if gemmaP95Ms > 0 && gemmaP95Ms <= 80.0 {
		res.Tier = TierBalanced
		res.ModelVersion = ModelVersionGemma
		res.P95Ms = gemmaP95Ms
		return res
	}

	// 规则 3：Contended 竞争状态，不落定论（本次暂用轻量档，下次重试）
	if contended {
		res.Tier = TierLight
		res.ModelVersion = ModelVersionBGE
		res.P95Ms = bgeP95Ms
		res.RetryOnStart = true
		return res
	}

	// 规则 4：轻量档达标 (<= 30ms)
	if bgeP95Ms > 0 && bgeP95Ms <= 30.0 {
		res.Tier = TierLight
		res.ModelVersion = ModelVersionBGE
		res.P95Ms = bgeP95Ms
		return res
	}

	// 规则 5：两档均超标，不挂嵌入器走 FTS
	res.Tier = TierNone
	res.ModelVersion = ""
	res.P95Ms = bgeP95Ms
	return res
}

// MeasureP95 运行 64 条样本并计算单条执行耗时的 p95（毫秒）。
func MeasureP95(ctx context.Context, inferFn func(ctx context.Context, text string) error) (float64, error) {
	samples := BenchSampleTexts()
	durations := make([]float64, len(samples))
	for i, text := range samples {
		select {
		case <-ctx.Done():
			return 0, apperr.Wrap(apperr.CodeInternal, "embedonnx: benchmark cancelled", ctx.Err())
		default:
		}

		start := time.Now()
		if err := inferFn(ctx, text); err != nil {
			return 0, apperr.Wrap(apperr.CodeInternal, "embedonnx: benchmark infer failed", err)
		}
		durations[i] = float64(time.Since(start).Microseconds()) / 1000.0
	}

	sort.Float64s(durations)
	p95Idx := int(math.Ceil(0.95*float64(len(durations)))) - 1
	if p95Idx >= len(durations) {
		p95Idx = len(durations) - 1
	}
	return durations[p95Idx], nil
}
