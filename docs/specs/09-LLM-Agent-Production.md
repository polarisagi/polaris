# 09 LLM Agent 工程化生产要求

> 面向 AI 编码助手与 Code Reviewer。生成任何 Agent / LLM / Tool / Memory / RAG 相关代码前必须验证以下原则全部满足；违反任何一条均为合并阻断项。
> 本文件是 `00-Constitution.md` 的 Agent 工程化扩展，不重复已有规则（关联：R1.2→P-9、R1.3→P-5、R1.9→P-6、R1.16→A-02）。

## 二、核心陷阱目录（A 系列）

| # | 陷阱 | 典型表现 | 正确做法 |
|---|------|---------|---------|
| **A-01** | LLM 输出零兜底 | `resp.Choices[0].Message.Content` 直用不判空 | 每次解析 LLM 响应都有格式兜底（空串、非预期类型、截断 JSON） |
| **A-02** | 同步阻塞 LLM 调用 | 主线程 / DB 事务内直接 `Infer()` | 持有 DB 连接期间严禁调 LLM（R1.16）；LLM 调用在独立 goroutine + `context.WithTimeout` 中完成 |
| **A-03** | 工具参数不校验 | LLM 原始 JSON 直传工具 | 执行前过 `SchemaValidateInterceptor`；工具内部亦做参数断言 |
| **A-04** | Failover 只剔一个 | 换一个备用 Provider 就放弃 | **全量遍历**可用 Provider 直到耗尽（`InferenceRouter.failover`，`internal/llm/router_failover.go`） |
| **A-05** | 超时依赖调用方 | 假设上层已设 deadline | 每个 LLM 适配器**自持超时**（普通 90s，流式 180s），不信任上层 ctx |
| **A-06** | 幂等缓存无界增长 | `map[string]result` 只写不淘汰 | LRU + TTL 双控（容量上限 + 过期），禁止裸 map 存全局状态 |
| **A-07** | RAG 只过一路召回 | 只有 BM25 或只有向量，无 Rerank | 混合检索（BM25 + Dense）+ RRF 融合 + 可选 Rerank；单路召回上线前须 Baseline 对比 |
| **A-08** | 图检索内容缺失 | Graph 节点 ID 当 Content 用 | 图检索结果**二次 KV 查询**补全原始 Payload |
| **A-09** | FSM 锁内做 IO | 状态机持全局锁期间调 LLM / 网络 | 耗时操作（LLM 推理、技能加载、扩展调用）移出锁范围，`SafeGo` 异步执行 |
| **A-10** | 熔断器裸读状态 | 并发下无锁读熔断状态字段 | 熔断器全部字段读写在 `mu.Lock()` / `mu.RLock()` 下 |
| **A-11** | 错误被 Saga 吞掉 | 补偿失败静默丢弃，状态机看不到回滚异常 | 补偿失败经 `apperr.Wrap` 上报，并用 `S_ROLLBACK_PARTIAL` 等中间状态标记 |
| **A-12** | Prompt 硬编码在业务层 | `"你是一个助手..." + userInput` 散落全库 | Prompt 统一在 `internal/prompt/` 管理；变量用结构化模板参数注入，不拼接用户输入 |
| **A-13** | goroutine 泄漏 | 启动 goroutine 无退出机制 | 每个 goroutine 监听 `ctx.Done()` 或停止 channel；经 `concurrent.SafeGo` 启动（捕获 panic）。门控：`Test_inv_NoBareGoroutine` |
| **A-14** | 向量扫描无上限 | `SELECT ... LIMIT 999999` 全表余弦 | Tier0 扫描量经 `RetrievalConfig.Tier0VectorScanLimit` 设上限（默认 500），防 OOM |

## 三、生产工程化核心原则（P 系列）

- **P-1 · 每次 LLM 调用必有超时保护**：`ctx, cancel := context.WithTimeout(ctx, 90*time.Second); defer cancel()` 后再 `provider.Infer(ctx, req)`；不依赖上层 ctx（同 A-05）。
- **P-2 · LLM 响应解析必有格式兜底**：`len(resp.Choices)==0 || Content==""` → `apperr.New(apperr.CodeInternal, "llm: empty response")`，不得直接索引（同 A-01）。
- **P-3 · 工具参数进执行链前校验 JSON 合法性**：链路 `Agent → Dispatcher → SchemaValidateInterceptor → PolicyGate → Sandbox → Executor`；拦截器在沙箱层前对非法 args 短路返回 `CodeInvalidInput`；新增工具绕过此链路均违规。
- **P-4 · 混合检索追踪每路召回来源**：每个 `ScoredFragment` 携带 `ExplainBits` 位图，标记 BM25 / Vector / Graph / Semantic 的贡献，是 RAG 调优的最小可观测单元。
- **P-5 · 缓存必有容量上限和过期淘汰**：LRU + TTL，禁止裸 map 无界增长（同 A-06）。
- **P-6 · Agent 状态机是唯一控制流持有者**：LLM 只推理、不控制跳转；状态跳转只经 FSM `Trigger()`，回调内不得直接改状态字段；长耗时操作（扩展激活、反射写回）异步，不得在锁内（同 A-09，R3-HE5）。
- **P-7 · Failover 全量遍历**：按优先级权重遍历全部备用 Provider，全部失败才向上报错（同 A-04）。
- **P-8 · 所有外部调用暴露 Prometheus 指标**：LLM、Tool、Embedding、Retrieval、Policy 评估每条链路都有：调用总数计数器（label `status=success/error`）、延迟直方图（ms）、失败计数（label `error_code`）。
- **P-9 · 错误全链路语义化**：一律 `apperr.Wrap / apperr.New`，携带 `Code` 与语义消息（如 `"retrieval: graph kv lookup failed"`），便于 HTTP 状态码路由、熔断判断与日志检索；禁止裸 `return nil, err`。

## 四、RAG 链路检查清单

检索相关代码合并前逐项确认：

- [ ] 文档切片大小与 overlap 经 Baseline 实验确定（禁止拍脑袋硬编码）
- [ ] Embedding 模型与维度和检索时一致（否则余弦空间错位）
- [ ] `Tier0VectorScanLimit` 在配置中明确设置（默认 500，大数据集调大）
- [ ] Graph 节点经 KV 二次查询还原原始 Payload（A-08）
- [ ] BM25/Vector/Graph 的 RRF 权重有实验依据
- [ ] 每路召回正确设置 `ExplainBits` 并上报 Prometheus
- [ ] 高精度场景接入 Rerank（Tier1+ 生产场景默认开启）

## 五、并发安全检查清单

- [ ] 每个 `goroutine` 有退出机制（`ctx.Done()` 或 channel）
- [ ] 共享状态受 `sync.Mutex` / `sync.RWMutex` 保护，无裸字段并发读写
- [ ] `make test-race` 通过
- [ ] FSM 状态跳转只经 `Trigger()`，未绕过锁直接赋值
- [ ] DB 事务内无 LLM / HTTP 外部调用（R1.16）
