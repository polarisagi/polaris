# 09 LLM Agent 工程化生产要求

> **本文件面向 AI 编码助手（以及人类 Code Reviewer）。**
> 在为本项目生成任何 Agent / LLM / Tool / Memory / RAG 相关代码前，
> 必须验证所有原则均被满足。违反任何一条均为合并阻断项（Merge-Blocker）。

---

## 一、为什么需要单独的「Agent 工程化原则」？

传统后端的稳定性套路（限流、熔断、幂等）在 LLM Agent 场景下被反复踩坑，根因在于：

1. **LLM 调用的不确定性**：同一 Prompt 可能返回不同格式、不同长度、甚至空响应。
2. **工具调用的副作用**：Agent 主导的 Function Calling 一旦携带脏参数就会破坏下游状态。
3. **多路并发复杂度**：多 Agent / 多 Provider / 多 Tool 并发是常态，锁与异步稍有不慎便会死锁或竞态。
4. **RAG 链路的精度陷阱**：Demo 能跑通 ≠ 生产可用；切片、向量、Rerank 的任何一环劣化都会导致检索精度断崖式下跌。
5. **可观测性盲区**：Agent 的每一跳（LLM → Tool → Memory → LLM）若无链路追踪，线上故障便无从定位。

---

## 二、核心陷阱目录（A 系列 Anti-Patterns）

| # | 陷阱名 | 典型表现 | 正确做法 |
|---|--------|---------|---------|
| **A-01** | LLM 输出零兜底 | `resp.Choices[0].Message.Content` 直接使用，不判空 | 每次解析 LLM 响应都必须有格式兜底（空串、非预期类型、截断 JSON）|
| **A-02** | 同步阻塞 LLM 调用 | 主线程 / DB 事务内直接 `Infer()` | **持有 DB 连接期间严禁发起 LLM 调用**（见 R1.16）；LLM 调用必须在独立 goroutine + `context.WithTimeout` 中完成 |
| **A-03** | 工具参数不校验 | Agent 把 LLM 原始 JSON 直传工具，未做 Unmarshal 校验 | 执行前必须过 `SchemaValidateInterceptor`；工具实现内部亦需参数断言 |
| **A-04** | Failover 只剔一个 | Provider 失败后换一个备用就放弃 | Failover 必须**全量遍历**所有可用 Provider 直到耗尽（见 `InferenceRouter.failover` 实现） |
| **A-05** | 超时依赖调用方 | 假设上层已设 `context deadline`，自己不再设 | 每个 LLM 适配器层**必须自持超时**（普通调用 90s，流式 180s），不信任上层传入的 ctx 一定带超时 |
| **A-06** | 幂等缓存无界增长 | `map[string]result` 只写不淘汰 | 必须使用 LRU + TTL 双控缓存（容量上限 + 时间过期），禁止裸 map 存全局状态 |
| **A-07** | RAG 只过一路召回 | 只有 BM25 或只有向量，无 Rerank | 混合检索（BM25 + Dense Vector）+ RRF 融合 + 可选 Rerank 重排；任何单路召回上线前必须 Baseline 对比 |
| **A-08** | 图检索内容缺失 | Graph 节点 ID 直接当 Content 使用 | 图检索结果必须**二次 KV 查询**补全原始内容，不得以 ID 代替 Payload |
| **A-09** | FSM 锁内做 IO | Agent 状态机持有全局锁期间调用 LLM / 网络 | 耗时操作（LLM 推理、技能加载、扩展调用）必须移出锁范围，以 `SafeGo` 异步执行 |
| **A-10** | 熔断器裸读状态 | 并发场景下无锁读熔断状态字段 | 熔断器所有字段读写必须在 `mu.Lock()` / `mu.RLock()` 保护下完成，禁止裸字段访问 |
| **A-11** | 错误被 Saga 吞掉 | 补偿失败静默丢弃，状态机看不到回滚异常 | 补偿失败必须通过 `apperr.Wrap` 包装上报，并引入 `S_ROLLBACK_PARTIAL` 等中间状态标记 |
| **A-12** | Prompt 硬编码在业务层 | `prompt := "你是一个助手..." + userInput` 散落全代码库 | Prompt 必须统一在 `internal/prompt/` 管理；变量部分用结构化模板参数注入，不得字符串拼接用户输入 |
| **A-13** | goroutine 泄漏 | 启动 goroutine 无退出机制 | 每个 goroutine 都必须监听 `ctx.Done()` 或通过 channel 接收停止信号；使用 `concurrent.SafeGo` 封装 panic 捕获 |
| **A-14** | 向量扫描无上限 | `SELECT ... LIMIT 999999` 全表余弦 | Tier0 向量扫描量必须通过 `RetrievalConfig.Tier0VectorScanLimit` 配置上限（默认 500），防止全表扫描导致 OOM |

---

## 三、生产工程化核心原则（P 系列 Principles）

### P-1 · 每次 LLM 调用必须有超时保护

```go
// ✅ 正确
ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
defer cancel()
resp, err := provider.Infer(ctx, req)

// ❌ 错误（依赖上层 ctx，上层可能无 deadline）
resp, err := provider.Infer(ctx, req)
```

### P-2 · LLM 响应解析必须有格式兜底

```go
// ✅ 正确
if len(resp.Choices) == 0 || resp.Choices[0].Message.Content == "" {
    return nil, apperr.New(apperr.CodeInternal, "llm: empty response")
}

// ❌ 错误
content := resp.Choices[0].Message.Content // 可能 panic 或空字符串透传
```

### P-3 · 工具参数在进入执行链前必须校验 JSON 合法性

工具调用链路：`Agent → Dispatcher → SchemaValidateInterceptor → PolicyGate → Sandbox → Executor`

`SchemaValidateInterceptor` 负责拦截格式非法的 args（非合法 JSON），在沙箱层之前短路返回 `CodeInvalidInput`。任何新增的工具绕过此链路均为违规。

### P-4 · 混合检索必须追踪每路召回来源

每个 `ScoredFragment` 必须携带 `ExplainBits` 位图，标记它是 BM25 / Vector / Graph / Semantic 等哪几路检索的贡献。这是 RAG 链路调优的最小可观测单元。

### P-5 · 缓存必须有容量上限和过期淘汰

```go
// ✅ 正确：LRU + TTL
type lruCache struct {
    cap  int
    ttl  time.Duration
    mu   sync.Mutex
    ...
}

// ❌ 错误：裸 map 无界增长
cache := make(map[string]Result)
```

### P-6 · Agent 状态机是唯一的控制流持有者

- LLM 是协处理器，只负责推理，**不控制流程跳转**
- 所有状态跳转必须通过 FSM `Trigger()` 驱动，不得在回调中直接修改状态字段
- 长耗时操作（扩展激活、反射写回）必须异步，不得在锁保护范围内进行

### P-7 · Failover 必须全量遍历，不得单次降级放弃

Provider 失败后的回退策略：遍历所有备用 Provider（按优先级权重），直到全部尝试失败才向上报错，禁止"换一个就放弃"的浅层降级。

### P-8 · 所有外部调用必须暴露 Prometheus 指标

每条链路（LLM 调用、Tool 调用、Embedding、Retrieval、Policy 评估）都必须有：

| 指标类型 | 必须有 |
|---------|------|
| 计数器 | 调用总次数（含 label: status=success/error） |
| 直方图 | 延迟分布（ms） |
| 错误计数 | 失败次数（含 label: error_code） |

### P-9 · 错误必须全链路语义化

```go
// ✅ 正确
return nil, apperr.Wrap(apperr.CodeInternal, "retrieval: graph kv lookup failed", err)

// ❌ 错误（裸 error 丢失上下文）
return nil, err
```

所有错误必须通过 `apperr.Wrap / apperr.New` 包装，携带 `Code` 和语义消息，便于上层路由（HTTP 状态码、熔断判断）和日志检索。

---

## 四、RAG 链路专项检查清单

在任何检索相关代码合并前，必须逐项确认：

- [ ] **文档切片**：切片大小与 overlap 是否经过 Baseline 实验确定？（禁止拍脑袋硬编码）
- [ ] **Embedding 模型**：是否与检索时使用同一模型和维度？（不一致会导致余弦空间错位）
- [ ] **向量扫描上限**：`Tier0VectorScanLimit` 是否在配置中明确设置？（默认 500，大数据集需调大）
- [ ] **图检索内容补全**：Graph 节点是否通过 KV 二次查询还原原始 Payload？（禁止用 ID 代替 Content）
- [ ] **RRF 融合权重**：BM25/Vector/Graph 各路权重是否有来自实验的依据？
- [ ] **ExplainBits 打点**：每路召回是否正确设置了对应的 bit 并上报 Prometheus？
- [ ] **Rerank**：高精度场景是否接入了 Rerank 重排？（Tier1+ 的生产场景应默认开启）

---

## 五、并发安全检查清单

- [ ] 所有 `goroutine` 启动是否有退出机制（`ctx.Done()` 或 channel）？
- [ ] 共享状态是否通过 `sync.Mutex` / `sync.RWMutex` 保护？（禁止裸字段并发读写）
- [ ] 是否通过 `make test-race` 验证无数据竞争？
- [ ] FSM 状态跳转是否只通过 `Trigger()` 发起，未绕过锁直接赋值？
- [ ] DB 事务内是否存在 LLM / HTTP 外部调用？（违反 R1.16，必须拆分）

---

## 六、与宪法规则的关联

本文件是 `00-Constitution.md` 的 **Agent 工程化扩展**，不重复定义已有规则，而是针对 LLM Agent 场景补充具体落地要求：

| 宪法规则 | 本文件扩展 |
|---------|----------|
| R1.2 裸 error 传播 | P-9 所有 Agent 调用链路均需 apperr 包装 |
| R1.3 全局可变变量 | P-5 缓存必须 LRU+TTL，不得裸 map 全局增长 |
| R1.9 LLM 自由流转 | P-6 FSM 是唯一控制流持有者 |
| R1.16 事务内外部调用 | P-1/A-02 LLM 调用禁止在 DB 锁持有期间发起 |

---

*最后更新：2026-07-09 / 来源：stability_analysis.md P0~P2 稳定性修复经验总结；规范审查校验*
