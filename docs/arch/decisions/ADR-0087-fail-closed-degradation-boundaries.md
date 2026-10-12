# ADR-0087: Fail-Closed 与禁止静默降级：通用规则、三个降级实例、八条判定规则与 Lint 门控

> 状态：Accepted ｜ 首次：2026-08-01 ｜ 最近复核：2026-10-12 ｜ 含原 ADR-0094 ｜ 模块：全局（`internal/security/`、`internal/sandbox/`、`internal/gateway/`、`internal/bootstrap/`、`internal/lint/`）

## 旧引用映射（代码注释里的「ADR-0094 决策N」按此跳转）

| 旧引用 | 新节 |
|---|---|
| 0087 通用规则 | 决策一 |
| 0087 实例 1/2/3 | 决策二 |
| 0094 决策一（Fail-Closed 三态） | 决策三 |
| 0094 决策二（身份单源） | 决策四 |
| 0094 决策三（生命周期锚定） | 决策五 |
| 0094 决策四（落盘不吞错） | 决策六 |
| 0094 决策五（FFI KeepAlive） | 决策七 |
| 0094 决策六（禁字符串直拼） | 决策八 |
| 0094 决策七（包装器透传） | 决策九 |
| 0094 决策八（封闭枚举） | 决策十 |

## 决策一：任何降级必须显式（原 ADR-0087 通用规则）

- 任何 fallback 必须有 counter + 明确日志，禁止静默；引入降级路径前须回答"谁能看见它发生了"，答案是"没人，除非翻源码"则不合格。
- 反例（已发现的静默降级）：可信来源 Wasm 不可用 → 静默 InProcess；ollama StreamInfer 静默跳过 TokenBurnRate；summary_gen 静默写 `taint_level=0`；22 处 `_ =` 静默吞没。共同模式：无 counter、无 Error 日志、无配置开关。
- 凡符合上述模式（静默切换执行路径 / 静默降低安全等级 / 静默丢弃错误）的新代码，审查一律驳回，除非同时补 counter + Error 日志 +（适用时）显式开关。

## 决策二：三个 fail-closed 实例（原 ADR-0087 实例 1/2/3）

1. **沙箱可信来源 InProcess 回退需显式 opt-in**：TrustOfficial 及以上在 Wasm/Container/Remote 均不可用时默认不降级 InProcess（`allow_trusted_inprocess_fallback=false`，`state.yaml sbx_allow_trusted_inprocess_fallback`）。可信 ≠ 稳定。完整决策见 ADR-0008 决策六（不在此重复）。实现 `internal/sandbox/sandbox_router.go`。
2. **cedarLeaks 用时间窗而非累计阈值**：`internal/security/policy/gate.go` 以滑动窗口 `cedarLeakWindow`=30min 内有效泄漏数判定，窗口内达 `cedarLeakKillSwitchThreshold`=5 才触发 KillSwitch；`cedarLeaksTotal`（只增不减）单独暴露为 Gauge `polaris_cedar_ffi_leaks_total` 供趋势观测，两计数器互不干扰；惰性衰减（记录时顺带清理），无后台 goroutine。
3. **PII 映射 LRU + 分区回收**：`internal/security/guard/pii_desensitizer.go` 按分区（通常=SessionID）有界 LRU：单分区 `piiMappingMaxEntries`=10000，分区数 `piiPartitionMaxEntries`=256，超限淘汰最久未用；被淘汰原值再现会得新假值（一致性仅窗口内保证）；无 SessionID 落 `piiGlobalPartition="global"`（受条目上限约束，不能被 `ReleasePartition` 精确回收）。淘汰日志按 `evictLogSampleN`=100 采样，`metrics.RecordPIIMappingEviction` 每次计数。阈值组 `cedar_gate`/`pii`/`sandbox` 见 `state.yaml`。

## 决策三：Fail-Closed 三态安全判定（原 ADR-0094 决策一）

- 返回"是否放行/风险等级"的函数必须区分 allow / deny / undetermined，undetermined 按 deny 处置；解析失败或异常路径禁止返回最低风险或隐式放行。
- 反例：`security_audit_agent.parseAuditResult` 无 JSON 时返回 `RiskLevel:"none"`。
- 已落实的依赖缺失即拒绝点：`ShadowExecutor.scoreShadow`（judge provider 未注入拒评）、`KnowledgeBase.Search`（`TaintMax` 零值拒检索）、`ContextRefExpander.resolveFile`（`workDir` 未配置拒绝）、`run_command`/`bash`（HITL 网关未注入拒绝，不回退执行）、`NewMCPMarketplaceClient`（拒绝未经 `SafeDialer` 包装的客户端）。
- 门控：`internal/lint/security_lint_test.go` `TestFailClosedSafetyVerdict`。

## 决策四：身份判定单源与 Authenticated 显式断言（原 ADR-0094 决策二）

- `authcontext.AuthContext.Authenticated`；HTTP handler 禁止用 `UserID == ""` / `UserID != "admin"` 猜测未认证，一律读 `Authenticated` 或经网关 ACL 中间件。`FromContext` 永不返回 nil，未认证时为 `UserID:"anonymous", Authenticated:false`。
- 反例：`admin_killswitch.HandleKill` 检查 `UserID == ""` 恒假，未授权请求越权进入 killswitch。
- 门控：`internal/gateway/server/sysadmin/admin_killswitch_test.go`（anonymous 与 admin 场景）。

## 决策五：生命周期锚定与 Channel/Ticker 监听（原 ADR-0094 决策三）

- `internal/` 内禁止用 `context.Background()` 派生生存期超过单次调用的 goroutine；长驻任务继承 `Bootstrapper.RootContext()`（`internal/bootstrap`）。带 `time.Ticker` 的循环与 long-lived goroutine 的 channel 发送必须在 `select` 中监听 `<-ctx.Done()`。
- 反例：`logic_collapse_trigger.go` 用 `context.Background()` 派生 10 分钟 context；`obsidian_connector.go` 裸写 channel 发送致协程泄漏。
- 门控：`internal/lint/bare_goroutine_test.go` 的 `TestNoBackgroundCtxInLongLivedGoroutine` / `TestTickerLoopHonorsCtxDone` / `TestChannelSendHonorsCtxDone`。

## 决策六：状态落盘不得静默吞错（原 ADR-0094 决策四）

- 持久化写失败（`ExecContext`/`Update`/`Delete` 等）不得仅记日志并返回"看似成功"的伪值；须返回 `error` 或未变更旧值。确属 best-effort/遥测写入须在 `slog.Warn` 上方写中文注释理由。
- 反例：`edge_weight.go` `ReinforcePath` 写失败仅 Warn 却返回 `currentWeight + reinforceRate`。
- 门控：`internal/lint/errcheck_lint_test.go` `TestDBWriteFailureNotSilentlySwallowed`。

## 决策七：FFI 指针保活判据（原 ADR-0094 决策五）

- 仅当实参写为 `uintptr(unsafe.Pointer(x))`（转整数擦除 GC 可达性）时，FFI 调用后紧跟 `runtime.KeepAlive(x)`；形参为 `*T`/`unsafe.Pointer` 时编译器与 GC 保证存活，**禁止**冗余 `KeepAlive`。
- 反例：对形参为 `*byte`/`unsafe.Pointer` 的函数误加 `KeepAlive` 产生假阳性。
- 门控：`internal/lint/ffi_abi_lint_test.go` `TestFFIKeepAliveBoundary`（按调用点语法扫描 `internal/tool/sandbox`、`internal/ffi`；原名 `TestUintptrFFIArgHasKeepAlive` 从未实现）。

## 决策八：结构化载体禁字符串直拼（原 ADR-0094 决策六）

- JSON 用 `json.Marshal` + 具名结构体；FTS5 `MATCH` 参数过 `pkg/util.QuoteFTS5`（精确短语）/`QuoteFTS5Query`（多词逐词转义）；Prompt 非受信变量用 `prompt.NewRandomBoundary` 包裹。
- 反例（均已改）：`episodic_mem_overflow.go` 直拼 JSON；`graph_traverser.go`/`retriever.go`/`rag_retrieval.go`/`repo_chat.go` 裸 `MATCH ?`；`community_summarizer.go` 节点内容直拼 Prompt。
- 门控：`internal/lint/security_lint_test.go` `TestNoRawStringIntoStructuredSink`（SQL 拼接，范围限 `internal/store/audit/`，放大到全仓会误报编译期 const 拼接）+ `TestFTS5MatchArgsUseQuoteHelper`（全仓）。

## 决策九：stdlib 接口包装器必须透传底层能力（原 ADR-0094 决策七）

- 包装 `http.ResponseWriter` 的类型必须实现 `Unwrap() http.ResponseWriter`（供 `http.ResponseController` 识别 Flusher/Hijacker/`SetWriteDeadline`）；包装 `slog.Handler` 的类型其 `WithAttrs`/`WithGroup` 必须返回重新包装自身的实例。
- 反例：`LoggingResponseWriter` 无 `Unwrap()` 致 `SetWriteDeadline` 静默失效；`LogStore.WithAttrs` 丢弃 `LogStore`。
- 门控：`internal/lint/arch_lint_test.go` `TestResponseWriterWrapperImplementsUnwrap` / `TestSlogHandlerWrapperPreservesSelf`。

## 决策十：封闭枚举禁止裸字符串（原 ADR-0094 决策八）

- 模型池（`general`/`default`/`reasoning`，另有 `budget`）等封闭枚举必须在 `pkg/types` 定义具名类型与常量（`pkg/types/enums_llm.go`），禁止调用点字符串字面量或不存在的池名。
- 反例：4 处字面量 `"standard"` 被静默回落 default。
- 门控：`internal/lint/naming_and_size_test.go` `TestModelPoolEnumOnly`。
- 孤儿 doc comment（`docs/specs/00-Constitution.md §R7`）由 `internal/lint/doc_comment_name_test.go` `Test_inv_DocCommentOrphan` 门控。

## 已驳方案

| 方案 | 驳回理由 |
|---|---|
| cedarLeaks 继续用累计值只提高阈值 | 长期运行进程仍会缓慢逼近阈值误触发 KillSwitch；语义应为"最近是否密集失败" |
| PII 映射表无上限 + 定期整表 `Clear()` | 清空含活跃会话；`Clear()` 从未接入生产；分区级 LRU 只回收过期会话 |
| 三实例各自独立成 ADR | 共享同一治理原则，合并更利于引用 |

## 重新评估触发条件

- 新降级路径引入前必须先回答"谁能看见它发生了"。
- 重提 cedarLeaks 提阈值或 PII 整表 `Clear()`：须先证明原驳回理由（误判性 KillSwitch / 影响活跃会话）已不成立。

## 沿革

| 日期 | 变更 | 原编号 |
|---|---|---|
| 2026-08-01 | 三个 fail-closed 实例 + 通用规则（阶段03 R-01/R-02/R-05） | 0087 |
| 2026-08-09 | 八条规范性决策 + 对应 CI 门控 | 0094 |
| 2026-08-10 | 复核订正：KeepAlive 门控名、孤儿 doc comment 门控名、`ModelPool` 枚举所在文件、`TestNoRawStringIntoStructuredSink` 范围收窄 | 0094 |
| 2026-08-13 | 增补依赖缺失即拒绝点（决策三） | 0094 |
| 2026-10-12 | 0094 并入；wiring 棘轮基线现位于 `tools/baselines/wiring-allowlist.txt`（L-13） | — |

## 引用代码

- `internal/sandbox/sandbox_router.go`、`internal/security/policy/gate.go`、`internal/security/guard/pii_desensitizer.go`、`internal/security/token/capability_token.go`
- `internal/gateway/authcontext/context.go`、`internal/gateway/server/sysadmin/admin_killswitch.go`、`internal/gateway/server/middleware.go`、`internal/gateway/server/logstream.go`
- `internal/bootstrap/bootstrapper.go`、`internal/memory/graph/edge_weight.go`、`internal/tool/sandbox/rust_native_sandbox.go`、`pkg/util/fts5.go`、`pkg/types/enums_llm.go`
- `internal/lint/`（各门控）、`tools/baselines/wiring-allowlist.txt`
