# 01 Go 编码规范

> 本项目独有的 Go 风格与结构要求，不含通用 Go 语言知识。

## F1 文件结构

每个 `.go` 文件按序排列：包级注释（仅 `doc.go` 写，其余不重复）→ `package` → `import` → `const` → `type` → 结构体 → `NewXxx` → 公开方法 → 私有方法（按被调用顺序）→ 辅助纯函数/转换函数。

例外：`xxx_test.go` 按被测对象分组；`doc.go` 只含包级 markdown 注释。参考 `internal/security/guard/factuality_guard.go`。

## F2 接口定义

接口在消费方定义（R1.4），注解标明生产者与消费方：

```go
// @consumer: M5(Memory System - 四层记忆物理存储)
// @producer: internal/store/(SQLite / SurrealDB-Core 引擎适配器)
// @arch: docs/arch/M02-Storage-Fabric.md §1.1
type Store interface { Get(ctx context.Context, key []byte) ([]byte, error) }
```

消费方接口放 `internal/protocol/interfaces_*.go`；实现方只暴露结构体与构造函数。

## F3 错误处理

- 唯一错误类型 `pkg/apperr/apperr.go` 的 `Error`；业务代码禁用裸 `error`、`fmt.Errorf`、`errors.New`（R1.2）
- 传播：`return apperr.Wrap(apperr.CodeInternal, "用户可读的描述", err)`；Code 常量见 `00 R2.5`
- 降级路径：`if err != nil { slog.Warn("降级描述"); fallback() }`，不 panic、不静默

参考 `internal/automation/resource_governor.go`。

## F4 并发模式

| 场景 | 模式 | 示例 |
|------|------|------|
| 保护结构体字段 | `sync.Mutex` 嵌入 | `Orchestrator.mu` |
| 多读少写 | `sync.RWMutex` | — |
| 阻塞等待 | `sync.Cond` + 独立 goroutine 监听 ctx | `ResourceGovernor.cond` |
| 事件驱动 | channel + select | — |
| 并发上限 | buffered chan 作 semaphore | `internal/learning/engine.go` `sem` |
| 定时触发 | `time.NewTicker` + `select` | `internal/learning/engine.go` `midTicker` |

信号量模板：`select { case sem <- struct{}{}: go func(){ defer func(){ <-sem }(); /*工作*/ }() ; default: /*满则降级，非阻塞丢弃*/ }`。goroutine 一律经 `concurrent.SafeGo`（门控：`Test_inv_NoBareGoroutine`；生命周期见 `09 A-13`）。

## F5 构造函数

- `NewXxx(cfg, deps...) *Xxx`，依赖全为构造参数
- 禁止 `SetXxx` 后置注入；Tier1+ 可选注入的 `InjectLLMProvider` 是被允许的特例
- `internal/` 业务包禁止 `init()`（仅 `cmd/` 入口允许）

## F6 导入顺序

三块、空行分隔：标准库 → 第三方 → `github.com/polarisagi/polaris/...`（internal / pkg）。

## F7 可测试性

- 消费方接口可 mock；测试文件与被测文件同级同包；表驱动
- 私有函数通过公开接口间接测试，除非逻辑复杂需直接单测

## F8 日志规范（XR-08）

唯一日志器 `log/slog`（结构化）；禁止 `fmt.Printf` / `log.Printf`（R1.12，门控 `Test_inv_NoBareLogPrint`）。示例：`slog.Warn("storage: migration skipped", "file", filename, "err", err)`。

**必选 key**（全仓统一，新增须先补入本表）：

| key | 类型 | 含义 |
|-----|------|------|
| `"err"` | error | 错误值（Warn/Error 必带） |
| `"agent"` | string | AgentID |
| `"task_id"` | string | TaskID |
| `"session"` | string | SessionID |
| `"provider"` | string | Provider 名称 |
| `"file"` | string | 文件路径 |
| `"latency_ms"` | int64 | 耗时毫秒 |

| 级别 | 场景 |
|------|------|
| `Error` | 需人工介入的故障（数据丢失、服务不可用） |
| `Warn` | 可降级/自动恢复的异常（超时重试、fallback 触发） |
| `Info` | 启动/关闭/状态变更/里程碑（事件级，非每请求） |
| `Debug` | 高频热路径细节（每请求级，生产默认关闭） |

## F9 HTTP Handler 规范（`internal/gateway/server`）

四段式：① 解析输入（唯一做 input validation 处）→ ② 调业务方法（`s.doXxx(ctx, req)`，handler 内不写 SQL）→ ③ 错误 `slog.Warn("server: handleXxx failed", "err", err)` 后返回错误响应 → ④ 设 `Content-Type: application/json` 并编码响应。标杆见 `07` 的 HTTP Handler 行。

禁止：handler 内直接 `db.ExecContext`（门控：`Test_inv_NoRawDBExecWriteInGateway`）；handler 内调 `protocol.Provider.Infer`（LLM 调用不属于 HTTP 层）；把裸 Go error 消息返给客户端（用户可读错误须单独定义）。

## F10 Context 传播

- 函数第一参数必为 `ctx context.Context`（测试 helper 除外）
- 禁止 `internal/` 业务路径用 `context.Background()`（仅 `cmd/`、test、cleanup goroutine 允许）
- 跨 goroutine 必须传入或派生 ctx，不得脱离父生命周期
- 调 LLM / 外部 HTTP / DB 必须 `context.WithTimeout` 设 deadline（LLM 适配器自持超时见 `09 A-05`）
