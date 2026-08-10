# ADR-0094: Fail-Closed 安全判定与生命周期锚定 Lint 门控

- **状态**: Accepted | **日期**: 2026-08-09 | **模块**: 全局 (`internal/security/`, `internal/gateway/`, `internal/bootstrap/`, `internal/lint/`)

## 背景

Polaris 架构要求遵循 `CLAUDE.md §不变量`（HE-1 ~ HE-7）。在系统加固与审查中发现若干由于哑默认值、fail-open 判定、后台协程脱锚、状态落盘吞错、字符串直拼及 FFI 语义混淆导致的缺陷。本 ADR 明确 8 条规范性决策及其对应 CI 机械化门控，确保相关反模式无法通过 CI。

## 决策一：Fail-Closed 三态安全判定

返回"是否放行"或"风险等级"的安全判定函数，其返回值必须能区分 allow / deny / undetermined 三态；undetermined 必须按 deny（最高风险）处置。禁止在解析失败或异常路径返回最低风险等级或隐式放行。

- **反例守护**：`security_audit_agent.parseAuditResult` 在无 JSON 时返回 `RiskLevel:"none"`；解析失败必须返回 `error` 拦截。
- **门控**：`internal/lint/security_lint_test.go` `TestFailClosedSafetyVerdict`。

## 决策二：身份判定单源与 Authenticated 显式断言

`authcontext.AuthContext` 增加 `Authenticated bool` 字段。HTTP handler **禁止**用 `UserID == ""` 或 `UserID != "admin"` 自行猜测未认证状态，一律读取 `Authenticated` 标记或通过网关 ACL 中间件进行拦截。

- **反例守护**：`admin_killswitch.HandleKill` 检查 `UserID == ""`，由于未配置 API Key 时 `FromContext` 返回 `UserID: "anonymous"` 哑值，导致 `== ""` 恒假，未授权请求越权进入 killswitch 路径。
- **门控**：在 `internal/gateway/server/sysadmin/admin_killswitch_test.go` 中针对 anonymous 与 admin 场景提供强单测覆盖。

## 决策三：生命周期锚定与 Channel/Ticker 监听

`internal/` 内不得直接使用 `context.Background()` 派生生存期超过单次调用的 long-running goroutine。长驻后台任务必须继承由 `internal/bootstrap` 单点下发的 `RootContext()`。所有带 `time.Ticker` 的循环体以及 long-lived goroutine 中的 channel 发送操作，必须在 `select` 中监听 `<-ctx.Done()`。

- **反例守护**：`logic_collapse_trigger.go` 中直接使用 `context.Background()` 派生 10 分钟超时 context；`obsidian_connector.go` 裸写 channel 发送导致 channel 阻塞时协程泄漏。
- **门控**：`internal/lint/bare_goroutine_test.go` 包含 `TestNoBackgroundCtxInLongLivedGoroutine`、`TestTickerLoopHonorsCtxDone` 和 `TestChannelSendHonorsCtxDone`。

## 决策四：状态落盘不得静默吞错

数据持久化写操作（`ExecContext`、`Update`、`Delete` 等）失败时，底层函数不得仅记日志并返回"看起来已成功"的伪值。函数必须显式返回 `error` 或返回未变更的旧值。若业务确定为 best-effort/遥测写入，必须在 `slog.Warn` 上方写明中文注释理由。

- **反例守护**：`edge_weight.go` 的 `ReinforcePath` 数据库写入失败仅 `slog.WarnContext`，却依然返回 `currentWeight + reinforceRate`。
- **门控**：`internal/lint/errcheck_lint_test.go` `TestDBWriteFailureNotSilentlySwallowed`。

## 决策五：FFI 指针保活判据与 KeepAlive 精确规则

Go purego/FFI 调用的指针保活判据：
1. 仅当实参静态类型写为 `uintptr(unsafe.Pointer(x))` 时，由于转换为整数擦除了 GC 指针可达性，必须在 FFI 调用后紧跟 `runtime.KeepAlive(x)`。
2. 当形参类型为 `*T` 或 `unsafe.Pointer` 时，Go 编译器和 GC 保证在底层函数调用期间指针实参存活，**禁止**无谓增加冗余 `runtime.KeepAlive`。

- **反例守护**：对形参为 `*byte` / `unsafe.Pointer` 的函数误加 `KeepAlive` 产生假阳性审计门控。
- **门控**：`internal/lint/ffi_abi_lint_test.go` `TestFFIKeepAliveBoundary`（2026-08-10 复核订正：原门控名 `TestUintptrFFIArgHasKeepAlive` 从未真正实现，实际函数名如上；判据同规则一（参数类型 vs 调用点 `uintptr(unsafe.Pointer(x))` 显式转换）二选一，本条按调用点语法特征扫描 `internal/tool/sandbox`、`internal/ffi`）。

## 决策六：结构化载体禁字符串直拼

JSON 序列化、SQL FTS5 `MATCH` 查询以及 LLM Prompt 拼接三类结构化载体，禁止用 `fmt.Sprintf` 等直拼未转义的运行时动态数据。
- JSON 统一使用 `json.Marshal` 与具名结构体；
- FTS5 `MATCH` 参数统一过 `pkg/util.QuoteFTS5`（精确短语）/`QuoteFTS5Query`（多词查询，逐词转义）转义；
- Prompt 非受信变量使用 `prompt.NewRandomBoundary` 包裹。

- **反例守护**：`episodic_mem_overflow.go` `"preview":%s` 直拼 JSON（已改用 `json.Marshal(episodicOverflowRef{...})`）；`graph_traverser.go`/`retriever.go`/`rag_retrieval.go`/`repo_chat.go` 裸 `MATCH ?` 直拼实体名/查询词（已改用 `pkg/util.QuoteFTS5`/`QuoteFTS5Query`）；`community_summarizer.go` 节点内容直拼 Prompt（已改用 `internal/prompt.NewRandomBoundary` 包裹）。
- **门控**：`internal/lint/security_lint_test.go` 的 `TestNoRawStringIntoStructuredSink`（SQL 拼接检查，范围限定 `internal/store/audit/`——2026-08-10 复核发现放大到全仓会对编译期 const 拼接产生误报，收窄回原范围）+ `TestFTS5MatchArgsUseQuoteHelper`（FTS5 转义检查，全仓覆盖）。

## 决策七：stdlib 接口包装器必须透传底层能力

所有包装 `http.ResponseWriter` 的类型必须实现 `Unwrap() http.ResponseWriter` 接口，以便 `http.ResponseController` 识别底层 `http.Flusher` / `http.Hijacker` / `SetWriteDeadline`。所有实现 `slog.Handler` 的包装类型，其 `WithAttrs` 和 `WithGroup` 方法必须返回**重新包装自身**的实例，禁止剥离包装层。

- **反例守护**：`LoggingResponseWriter` 未实现 `Unwrap()` 导致全链路 `SetWriteDeadline` 静默失效；`LogStore.WithAttrs` 丢弃 `LogStore` 导致子日志丢日志流。
- **门控**：`internal/lint/arch_lint_test.go` `TestResponseWriterWrapperImplementsUnwrap` 与 `TestSlogHandlerWrapperPreservesSelf`。

## 决策八：封闭枚举语义禁止裸字符串

模型池（`general` / `default` / `reasoning`）等封闭枚举必须在 `pkg/types` 中定义具名类型及常量，禁止在调用点直接使用字符串字面量或写不存在的池名（如 `"standard"`）。

- **反例守护**：全仓 4 处写字面量 `"standard"` 被静默回落到 default，缺乏编译期/类型约束。
- **门控**：`internal/lint/arch_lint_test.go` `TestModelPoolEnumOnly`。

> 注：孤儿 doc comment（沿用 `docs/specs/00-Constitution.md §R7`）在 `internal/lint/doc_comment_name_test.go` 的 `Test_inv_DocCommentOrphan` 补齐 AST 自动化门控（2026-08-10 复核订正：原文档写 `TestNoOrphanDocComment`，与实际函数名不符）。

## 引用代码

- `internal/gateway/authcontext/context.go`
- `internal/gateway/server/sysadmin/admin_killswitch.go`
- `internal/security/token/capability_token.go`
- `internal/bootstrap/bootstrapper.go`
- `internal/memory/graph/edge_weight.go`
- `internal/tool/sandbox/rust_native_sandbox.go`
- `internal/gateway/server/middleware.go`
- `internal/gateway/server/logstream.go`
- `pkg/types/enums_llm.go`（2026-08-10 复核订正：原文档写 `pkg/types/models_pool.go`，`ModelPool` 强类型枚举实际定义在此文件）
- `internal/lint/`
