# Polaris 第二轮修复与安全加固提示词
> 执行者：Gemini | 审查者：Claude
> 本文档是第一轮开发的后续补丁，专注于补全 stub 和新增代码级安全检查。

---

## 一、强制约束（与第一轮相同，必须遵守）

1. `make fmt && make lint && make test` 全绿才能提交
2. 禁 `go test ./...`，只用 `make test`
3. 错误统一用 `internal/errors` 包装
4. 禁全局可变变量（`pkg/` 目录）
5. Git 署名：`MrLaoLiAI <polarisagi.online@gmail.com>`
6. 代码注释中文，标识符英文
7. 100% 指令溯源——禁止顺手重构未涉及的文件

---

## 二、本轮任务总览

| # | 文件 | 类型 | 问题 |
|---|---|---|---|
| 1 | `pkg/swarm/agents/memory_agent.go` | 补全 stub | `distill()` 未实现，`Run()` 里注释掉了 |
| 2 | `pkg/swarm/agents/governance_agent.go` | 补全 stub + 新增 | `probeMemory` / `CheckIdempotent` / `RecordExecution` 是空函数；新增代码级安全检查 |
| 3 | `pkg/swarm/agents/extension_librarian_handler.go` | 补全 stub | `Handle()` 无任何业务逻辑 |
| 4 | `pkg/swarm/agents/semantic_compress_handler.go` | 补全 stub | `Handle()` 无任何业务逻辑 |
| 5 | `pkg/swarm/reflexion.go` | 补全 stub | `replaySuccess()` 拿到 insight 后直接 `_ = insight` 丢弃 |
| 6 | `pkg/swarm/planner/pool.go` | 补全 stub | Engine A（编译+单测）完全缺失 |
| 7 | `pkg/action/mock_proxy.go` + `pkg/action/sandbox_impl.go` | 重新实现 | MockProxy 实现方案错误，未接入沙箱 |
| 8 | `pkg/swarm/agents/governance_agent.go` | 新增安全模块 | Python/Bash AST 规则扫描 |
| 9 | `pkg/swarm/agents/governance_agent.go` 或新文件 | 新增安全模块 | Wasm Import Section 白名单校验 |

---

## 三、详细实现说明

### 任务 1：补全 MemoryAgent 蒸馏逻辑

文件：`pkg/swarm/agents/memory_agent.go`

#### 1a：实现 `distill()` 方法

```go
// distill 将 L1 冷数据蒸馏为 L2 事实三元组，写入 SurrealDB。
//
// 执行流程：
//   1. 查询 episodic_memory 表中 meta 字段包含 {"cold":true} 的记录，最多取 20 条
//      SQL: SELECT id, content FROM episodic_memory
//           WHERE json_extract(meta, '$.cold') = 1
//           AND distilled_at IS NULL
//           ORDER BY created_at ASC LIMIT 20
//   2. 将 20 条记录拼成一个 Prompt，调用 LLM（ma.llmInfer），要求输出 JSON 数组：
//      [{"subject":"...","predicate":"...","object":"..."}]
//      Prompt 模板：
//        "以下是来自 AI Agent 执行日志的片段，请提取其中的事实三元组。
//         每个三元组格式：{\"subject\": \"...\", \"predicate\": \"...\", \"object\": \"...\"}
//         只输出 JSON 数组，不要任何解释。\n\n日志片段：\n<内容>"
//   3. 解析 LLM 返回的 JSON，对每个三元组：
//      - 构造 docID = "triple_" + sha256(subject+predicate+object)[:16]
//      - 调用 ma.surreal.FTSIndex(docID, subject+" "+predicate+" "+object)
//      - 调用 ma.surreal.GraphRelate(subject, predicate, object, 1.0)
//   4. 更新 episodic_memory 表：将已处理记录的 meta 字段补充 {"cold":true,"distilled_at":<unix>}
//      SQL: UPDATE episodic_memory SET meta = json_patch(meta, '{"distilled_at": <unix>}')
//           WHERE id = ?
//   5. 若 LLM 调用失败（err != nil），记录日志，不更新 distilled_at（下次重试）
//
// Tier-0 约束：单次最多处理 20 条，LLM 调用超时 30s（通过 ctx 控制）
func (ma *MemoryAgent) distill(ctx context.Context) error
```

#### 1b：修改 `Run()` 方法

将 `// _ = ma.distill(ctx)` 这行改为真实调用：
```go
case <-ticker.C:
    if ma.memPressure != nil && ma.memPressure.Load() >= 2 {
        // 内存严重不足时跳过蒸馏，保护系统稳定性
        continue
    }
    if err := ma.distill(ctx); err != nil {
        slog.Error("memory_agent: distill failed", "err", err)
    }
```

---

### 任务 2：补全 GovernanceAgent 核心方法

文件：`pkg/swarm/agents/governance_agent.go`

#### 2a：实现 `probeMemory()`

```go
// probeMemory 探测系统空闲内存，更新 memPressure atomic。
//
// 实现策略（平台兼容）：
//   Linux：解析 /proc/meminfo，读取 MemTotal 和 MemAvailable 字段（单位 kB）。
//     grep 规则：`^MemTotal:` 和 `^MemAvailable:`
//   macOS：调用 `vm_stat` 命令，解析 "Pages free" 和 "Pages inactive"，
//     结合 pagesize（通常 4096 或 16384 字节）计算可用内存。
//   其他平台：读取 runtime.MemStats（仅 Go 堆，不精确，作为 fallback）
//
// 压力等级计算（freePct = MemAvailable / MemTotal）：
//   freePct > 0.30  → MemPressureNormal   (0)
//   0.10 < freePct ≤ 0.30 → MemPressureModerate (1)
//   freePct ≤ 0.10  → MemPressureCritical  (2)
//
// 注意：此函数在 5s 定时器里调用，不能阻塞，读取超时设 1s。
func (ga *GovernanceAgent) probeMemory()
```

#### 2b：实现 `CheckIdempotent()`

```go
// CheckIdempotent 查询 outbox 表，检查给定操作哈希是否已执行过。
//
// SQL：
//   SELECT payload FROM outbox
//   WHERE idempotency_key = 'idem:' || ?
//   AND status = 'done'
//   LIMIT 1
//
// 命中（status='done'）：返回 payload 字段内容（历史响应快照）和 true
// 未命中：返回 nil, false
//
// 说明：idempotency_key 前缀 'idem:' 与普通 outbox 任务隔离，避免 key 碰撞。
func (ga *GovernanceAgent) CheckIdempotent(ctx context.Context, operationHash string) ([]byte, bool)
```

#### 2c：实现 `RecordExecution()`

```go
// RecordExecution 将成功执行的操作及其响应写入 outbox，供下次幂等命中。
//
// SQL（INSERT OR IGNORE，幂等写入）：
//   INSERT OR IGNORE INTO outbox
//     (idempotency_key, target_engine, operation, scope, payload, status, created_at)
//   VALUES
//     ('idem:' || ?, 'idempotent_gateway', 'record', 'execution', ?, 'done', ?)
//
// payload = response 字节（原样存储）
// created_at = Unix 毫秒时间戳
func (ga *GovernanceAgent) RecordExecution(ctx context.Context, operationHash string, response []byte) error
```

---

### 任务 3：补全 Extension Librarian Handler

文件：`pkg/swarm/agents/extension_librarian_handler.go`

重写 `ExtensionLibrarianHandler` 结构体和 `Handle()` 方法：

```go
// ExtensionLibrarianHandler 在扩展安装后，将其能力索引到 SurrealDB 知识图谱。
// 使 Planner 等智能体能通过语义搜索快速定位最适合的扩展。
type ExtensionLibrarianHandler struct {
    db       *sql.DB
    surreal  SurrealWriterInterface  // 复用 agents 包已定义的接口
    llmInfer LLMInferFunc            // 复用 agents 包已定义的类型
    embedFn  EmbedFunc               // 文本向量化函数（可为 nil，nil 时跳过向量索引）
}

// EmbedFunc 文本向量化函数类型（依赖注入，nil 时跳过）
type EmbedFunc func(ctx context.Context, text string) ([]float32, error)

func NewExtensionLibrarianHandler(
    db *sql.DB,
    surreal SurrealWriterInterface,
    llmInfer LLMInferFunc,
    embedFn EmbedFunc,
) *ExtensionLibrarianHandler
```

`Handle()` 实现流程：

```
1. 解析 payload，取出 extension_id

2. 从 extension_instances 表查询元数据：
   SELECT name, publisher, install_path, config FROM extension_instances WHERE id = ?

3. 尝试读取扩展文档（按优先级顺序）：
   - <install_path>/README.md
   - <install_path>/AGENTS.md
   - <install_path>/schema.json
   任何一个存在就用，全部不存在则用 name + publisher 拼一段简短描述作为文档

4. 调用 LLM（llmInfer），输入扩展文档内容，Prompt：
   "请分析以下工具/扩展的文档，提取其核心能力，输出严格 JSON：
    {
      \"summary\": \"一句话描述（≤50字）\",
      \"capabilities\": [\"能力1\", \"能力2\", ...],
      \"best_for\": [\"适合场景1\", \"场景2\"],
      \"avoid_when\": [\"不适合场景1\"]
    }
    文档内容：<文档内容>"

5. 解析 LLM 返回的 JSON

6. 写入 SurrealDB：
   - docID = "ext_" + extension_id
   - indexText = summary + " " + strings.Join(capabilities, " ") + " " + strings.Join(best_for, " ")
   - FTSIndex(docID, indexText)
   - 对每个 capability：GraphRelate("extension:"+extension_id, "provides", "cap:"+capability, 1.0)
   - 若 embedFn != nil：向量化 summary，调用 VecUpsert(docID, embedding)

7. 更新 extension_instances 表，标记已索引：
   UPDATE extension_instances SET meta = json_patch(COALESCE(meta,'{}'), '{"librarian_indexed":true}') WHERE id = ?

8. 全程错误用 internal/errors 包装，任何步骤失败都返回 error（让 OutboxWorker 重试）
```

---

### 任务 4：补全 SemanticCompressHandler

文件：`pkg/swarm/agents/semantic_compress_handler.go`

重写结构体和 `Handle()` 方法：

```go
// SemanticCompressHandler 将 VFS 中的大型错误堆栈提炼为结构化 JSON，
// 保护 L0 工作记忆不被原始报错信息淹没。
type SemanticCompressHandler struct {
    db       *sql.DB
    llmInfer LLMInferFunc  // 复用 agents 包已定义的类型
    vfsBase  string        // VFS 文件根目录（如 ~/.polarisagi/polaris/data/vfs/）
}

func NewSemanticCompressHandler(db *sql.DB, llmInfer LLMInferFunc, vfsBase string) *SemanticCompressHandler
```

`Handle()` 实现流程：

```
1. 解析 payload，取出 vfs_id

2. 从 workspace_vfs 表查询文件路径：
   SELECT file_path, size FROM workspace_vfs WHERE id = ?

3. 读取原始文件内容（file_path 是相对于 vfsBase 的路径）
   若文件不存在，返回 nil（已被清理，跳过）

4. 截取前 8000 字节（避免 LLM token 超限），调用 llmInfer，Prompt：
   "以下是一段程序报错信息，请提取关键信息，输出严格 JSON，不要有任何解释：
    {
      \"root_cause\": \"根本原因（≤100字）\",
      \"error_type\": \"错误类型（如 NullPointerException, SegFault, OutOfMemory 等）\",
      \"suggest_fix\": \"修复建议（≤100字）\",
      \"affected_file\": \"涉及的主要文件路径（无则为空字符串）\"
    }
    报错内容：<前8000字节>"

5. 解析 LLM 返回的 JSON，若解析失败则使用兜底结构：
   {"root_cause": "解析失败，见原始文件", "error_type": "unknown", "suggest_fix": "", "affected_file": ""}

6. 将 JSON 写回 workspace_vfs 文件路径（覆盖原始内容）
   同时更新 workspace_vfs 表的 size 字段为新内容字节数

7. 更新 workspace_vfs meta 字段，标记已压缩：
   UPDATE workspace_vfs SET meta = json_patch(COALESCE(meta,'{}'), '{"semantic_compressed":true,"original_size":<N>}') WHERE id = ?
```

---

### 任务 5：补全 replaySuccess() SurrealDB 写入

文件：`pkg/swarm/reflexion.go`

找到 `replaySuccess()` 方法中的 `_ = insight`，替换为真实的写入逻辑：

```go
go func() {
    if re.llmInfer == nil {
        return
    }
    prompt := fmt.Sprintf(
        "以下是一次 AI Agent 任务的执行轨迹，该任务经过 %d 次重规划后最终成功。\n"+
        "任务类型：%s\n"+
        "请提炼这次'犯错→探索→成功'过程中最有价值的洞察，使未来类似任务能避免相同错误。\n"+
        "输出严格 JSON：{\"insight\": \"核心洞察（≤200字）\", \"tags\": [\"标签1\", \"标签2\"]}\n\n"+
        "执行轨迹：\n%s",
        replanCount, taskType, formatTrajectory(trajectory),
    )

    ctx2, cancel := context.WithTimeout(context.Background(), 30*time.Second)
    defer cancel()

    insight, err := re.llmInfer(ctx2, prompt)
    if err != nil {
        slog.Error("replaySuccess: llm infer failed", "task_id", taskID, "err", err)
        return
    }

    // 解析 insight JSON
    var parsed struct {
        Insight string   `json:"insight"`
        Tags    []string `json:"tags"`
    }
    if jsonErr := json.Unmarshal([]byte(insight), &parsed); jsonErr != nil || parsed.Insight == "" {
        slog.Warn("replaySuccess: insight parse failed, using raw", "task_id", taskID)
        parsed.Insight = insight
    }

    // 写入 SurrealDB FTS 索引
    if re.surreal != nil {
        docID := taskID + "_her"
        if ftsErr := re.surreal.FTSIndex(docID, parsed.Insight); ftsErr != nil {
            slog.Error("replaySuccess: FTSIndex failed", "err", ftsErr)
        }
        // 建立图关系：任务类型 → 从失败中学到 → 洞察节点
        weight := float64(replanCount)
        if graphErr := re.surreal.GraphRelate(
            "task_type:"+taskType,
            "learned_from_failure",
            "insight:"+docID,
            weight,
        ); graphErr != nil {
            slog.Error("replaySuccess: GraphRelate failed", "err", graphErr)
        }
    }

    // 写入 reflection_memory 表（SQLite）
    if re.db != nil {
        now := time.Now().Unix()
        _, sqlErr := re.db.ExecContext(context.Background(),
            `INSERT OR IGNORE INTO reflection_memory
             (id, task_id, task_type, reflection_type, content, created_at)
             VALUES (?, ?, ?, 'success_pattern', ?, ?)`,
            taskID+"_her", taskID, taskType, parsed.Insight, now,
        )
        if sqlErr != nil {
            slog.Error("replaySuccess: insert reflection_memory failed", "err", sqlErr)
        }
    }
}()
```

**同时确认**：`ReflexionEngine` 结构体需要有 `db *sql.DB` 和 `surreal SurrealWriterInterface` 两个字段（若无则新增），并在构造函数 `NewReflexionEngine` 中允许注入这两个字段（nil 时跳过对应写入）。

新增辅助函数 `formatTrajectory(trajectory []Step) string`：将 trajectory 里的 Step 格式化为可读文本（每步一行：`[step N] action: <action>, result: <result>`），最多取最后 10 步，避免 Prompt 过长。

---

### 任务 6：补全 PlannerPool Engine A

文件：`pkg/swarm/planner/pool.go`

当前 `pool.go` 只有 Engine B（LLM 规划）路径，且 3 个 worker 都是同一逻辑（仅 temperature 不同）。需要根据 `taskType` 分叉：

#### 6a：task_type = "code_act" → Engine A 路径

```go
// workerEngineA 负责 code_act 任务的编译+单测评分（Engine A）。
//
// 执行流程：
//   1. 调用 LLM 生成代码补丁（使用不同 system prompt 产生多样性）：
//      - Worker 0：system prompt 强调"最小修改，保持现有风格"
//      - Worker 1：system prompt 强调"正确性优先，允许重写"
//      - Worker 2：system prompt 强调"性能优先，可引入新依赖"
//
//   2. 将代码补丁写入临时目录（os.MkdirTemp）
//
//   3. 在 Wasmtime 沙箱中执行编译（DryRun 模式）：
//      若沙箱不可用（Tier-0 资源不足），fallback 到 exec.Command("go", "build", "./...")
//      编译超时：30s
//
//   4. 若编译成功（exit code 0），继续运行单测：
//      exec.Command("go", "test", "-run", ".", "-timeout", "20s", "./...")
//      注意：这里必须用 exec.Command 直接调用，不能用 `make test`（make 不在沙箱里）
//
//   5. 计算 CompileScore（0.0~1.0）：
//      - 编译失败：0.0
//      - 编译成功，无测试：0.5
//      - 编译成功，测试全过：1.0
//      - 编译成功，测试部分失败：0.5 + 0.5 * (passed / total)
//
//   6. 将结果封装为 protocol.MemoryWhisper，推送到 p.whisperChan：
//      Content = "[PLANNER_ENGINE_A] score=<X> patch=<代码补丁前200字节>"
//      Salience = CompileScore
//
// Tier-0 约束：3 个 worker 并发，但每个 worker 只占用一个 goroutine，
//   没有额外的内存开销。临时目录在 worker 结束后清理。
func (p *PlannerPool) workerEngineA(ctx context.Context, workerID int, resultChan chan<- workerResult)
```

#### 6b：修改 `Run()` 方法

```go
func (p *PlannerPool) Run(ctx context.Context) {
    if p.whisperChan == nil {
        return
    }

    // 根据 taskType 决定走 Engine A 还是 Engine B
    workerCount := 3
    resultChan := make(chan workerResult, workerCount)
    var wg sync.WaitGroup

    for i := 0; i < workerCount; i++ {
        wg.Add(1)
        go func(id int) {
            defer wg.Done()
            if p.taskType == "code_act" {
                p.workerEngineA(ctx, id, resultChan)
            } else {
                p.workerEngineB(ctx, id, resultChan)  // 原有逻辑重命名为 workerEngineB
            }
        }(i)
    }

    go func() { wg.Wait(); close(resultChan) }()

    // 收集所有结果，选得分最高的推送
    var best workerResult
    for res := range resultChan {
        if res.score > best.score {
            best = res
        }
    }
    if best.content != "" {
        select {
        case p.whisperChan <- protocol.MemoryWhisper{
            Content:  best.content,
            Source:   "planner_pool",
            Salience: best.score,
        }:
        case <-ctx.Done():
        }
    }
}

// workerResult 单个 worker 的产出。
type workerResult struct {
    score   float64
    content string
    engine  string // "engine_a" | "engine_b"
}
```

---

### 任务 7：重新实现 MockProxy 并接入沙箱

#### 7a：重写 `pkg/action/mock_proxy.go`

当前实现是一个工具拦截器（错误方案），需要完全重写为 HTTP 代理服务器：

```go
package action

// MockProxy 是 MCTS 试运行期间的本地 HTTP 代理服务器。
//
// 工作原理：
//   在 loopback 动态端口启动一个 HTTP 代理，沙箱通过 HTTP_PROXY 环境变量
//   将所有 HTTP 流量路由到此代理，代理根据 MockTable 返回预生成响应。
//   代理不转发任何请求到公网，所有未命中的请求返回 HTTP 200 + {}。
//
// 与 sandbox_impl.go 的集成：
//   SandboxSpec.DryRunMode = true 时，sandbox_impl 创建 MockProxy，
//   将 proxy.EnvVars() 注入沙箱的环境变量（HTTP_PROXY, HTTPS_PROXY）。
type MockProxy struct {
    mockTable map[string]MockResponse
    listener  net.Listener
    mu        sync.RWMutex
    server    *http.Server
}

// MockResponse 单条 Mock 响应定义。
type MockResponse struct {
    StatusCode int               `json:"status_code"` // 默认 200
    Body       json.RawMessage   `json:"body"`        // 响应体 JSON
    Headers    map[string]string `json:"headers"`     // 可选响应头
}

// NewMockProxy 创建并启动 MockProxy，监听 127.0.0.1:0（动态端口）。
// 返回 *MockProxy 和代理监听地址（如 "127.0.0.1:54321"）。
func NewMockProxy(mockTable map[string]MockResponse) (*MockProxy, string, error) {
    ln, err := net.Listen("tcp", "127.0.0.1:0")
    // ...
    mp := &MockProxy{mockTable: mockTable, listener: ln}
    mp.server = &http.Server{Handler: mp}
    go mp.server.Serve(ln)
    return mp, ln.Addr().String(), nil
}

// ServeHTTP 实现 http.Handler 接口，处理所有代理请求。
// 查找逻辑：key = SHA256(method + " " + url)[:16]（hex string）
// 命中：返回 MockResponse 定义的 status + body
// 未命中：返回 200 + {"mocked": true}
func (mp *MockProxy) ServeHTTP(w http.ResponseWriter, r *http.Request)

// EnvVars 返回需要注入沙箱的环境变量。
func (mp *MockProxy) EnvVars() map[string]string {
    addr := mp.listener.Addr().String()
    return map[string]string{
        "HTTP_PROXY":  "http://" + addr,
        "HTTPS_PROXY": "http://" + addr,
        "NO_PROXY":    "",
    }
}

// Close 优雅关闭代理服务器。
func (mp *MockProxy) Close() error
```

#### 7b：修改 `pkg/action/sandbox_impl.go`

在执行沙箱之前，检测 `SandboxSpec.DryRunMode`，若为 true 则：

```go
// DryRun 模式：创建 MockProxy 并注入环境变量
// 位置：在 sandbox 执行代码之前（exec.Command 创建之前）
if spec.DryRunMode {
    mockTable := make(map[string]MockResponse) // 空 mock 表作为默认（兜底返回 {}）
    proxy, proxyAddr, err := NewMockProxy(mockTable)
    if err != nil {
        return nil, perrors.Wrap(perrors.CodeInternal, "create mock proxy failed", err)
    }
    defer proxy.Close()
    _ = proxyAddr
    // 将 proxy.EnvVars() 注入到 cmd.Env
    for k, v := range proxy.EnvVars() {
        cmd.Env = append(cmd.Env, k+"="+v)
    }
}
```

---

### 任务 8：新增 Python/Bash AST 安全规则扫描

文件：`pkg/swarm/agents/governance_agent.go`（新增方法）或新建 `pkg/swarm/agents/code_validator.go`

```go
// ValidateCode 在代码进入沙箱前执行确定性安全规则扫描。
//
// 设计原则：
//   确定性检查，非概率过滤（符合 HE-Rule 2：可验证执行）。
//   无 LLM 调用，微秒级完成，不影响执行延迟。
//   目标：拦截"懒惰的"恶意代码模式，不保证覆盖所有混淆。
//   真正的物理隔离仍由 Wasmtime 沙箱 + CapabilityToken 提供。
//
// 参数：
//   language = "python" | "bash" | "wasm"（wasm 走 ValidateWasmImports，见任务9）
//   code     = 待检查的源代码字节
//   caps     = 当前任务持有的能力集合（来自 CapabilityToken）
//
// 返回：
//   nil = 通过检查
//   error = 检查不通过，包含具体违规原因（用 internal/errors 包装）
func (ga *GovernanceAgent) ValidateCode(language string, code []byte, caps CapabilitySet) error

// CapabilitySet 能力集合（来自 CapabilityToken 的解析结果）。
type CapabilitySet map[string]bool
```

#### Python 规则集（规则 ID + 检测方法 + 处理策略）

**使用 Go 实现 Python AST 解析**：引入轻量级 Go Python 解析库（推荐 `github.com/nicholasgasior/gopy` 或手写关键词+正则组合，如下方案更实用）。

**实用方案**：Python 代码的危险模式绝大多数可以通过 Go 的 `text/scanner` + 正则匹配到调用 AST 节点（函数名识别），不需要完整 AST 解析器：

```go
// pythonDangerousPatterns Python 危险调用规则表。
// key = 规则 ID，value = 检测逻辑描述
var pythonDangerousPatterns = []codeRule{
    {
        id:          "PY001",
        description: "动态代码执行：exec() / eval()",
        // 检测：在 token 流中找到 exec( 或 eval( 调用
        // 例外：能力集含 "dynamic_eval" 时允许
        requiredCap: "dynamic_eval",
        pattern:     regexp.MustCompile(`\b(exec|eval)\s*\(`),
    },
    {
        id:          "PY002",
        description: "动态导入：__import__()",
        requiredCap: "dynamic_import",
        pattern:     regexp.MustCompile(`__import__\s*\(`),
    },
    {
        id:          "PY003",
        description: "Shell 命令执行：os.system() / os.popen()",
        requiredCap: "shell_exec",
        pattern:     regexp.MustCompile(`os\.(system|popen|exec[lv][pe]?)\s*\(`),
    },
    {
        id:          "PY004",
        description: "子进程启动：subprocess 模块",
        requiredCap: "shell_exec",
        pattern:     regexp.MustCompile(`subprocess\.(run|call|Popen|check_output)\s*\(`),
    },
    {
        id:          "PY005",
        description: "网络 socket 原始调用",
        requiredCap: "network_raw",
        pattern:     regexp.MustCompile(`socket\.socket\s*\(`),
    },
    {
        id:          "PY006",
        description: "危险路径写入：写入 /etc、/sys、/proc、/boot",
        requiredCap: "system_write",
        // 匹配 open(path, 'w'/'wb'/'a') 中 path 含危险前缀
        // 用字符串扫描找 open( 后的字符串参数
        pattern:     regexp.MustCompile(`open\s*\(\s*["'](/etc|/sys|/proc|/boot|/root)`),
    },
    {
        id:          "PY007",
        description: "ctypes 内存直接操作（旁路沙箱）",
        requiredCap: "native_memory",
        pattern:     regexp.MustCompile(`\bctypes\b`),
    },
}
```

**规则引擎执行逻辑**：

```go
for _, rule := range pythonDangerousPatterns {
    if rule.pattern.Match(code) {
        if !caps[rule.requiredCap] {
            return perrors.Wrapf(perrors.CodeForbidden,
                "code validation failed: rule %s (%s) triggered, requires capability '%s'",
                rule.id, rule.description, rule.requiredCap)
        }
    }
}
```

#### Bash 规则集

```go
var bashDangerousPatterns = []codeRule{
    {id: "SH001", description: "危险递归删除", requiredCap: "destructive_fs",
        pattern: regexp.MustCompile(`rm\s+(-[rfR]+\s+)?(/|~|\$HOME|\*|\$\w+)`)},
    {id: "SH002", description: "格式化磁盘", requiredCap: "destructive_fs",
        pattern: regexp.MustCompile(`\b(mkfs|dd\s+.*of=/dev|shred)\b`)},
    {id: "SH003", description: "修改系统文件", requiredCap: "system_write",
        pattern: regexp.MustCompile(`\b(chmod|chown|chattr)\b.*(/etc|/sys|/boot)`)},
    {id: "SH004", description: "curl/wget 管道执行", requiredCap: "network_exec",
        pattern: regexp.MustCompile(`(curl|wget).*\|\s*(bash|sh|python)`)},
    {id: "SH005", description: "后台驻留进程", requiredCap: "background_process",
        pattern: regexp.MustCompile(`nohup\s|&\s*$|disown`)},
}
```

**`ValidateCode` 主方法结构**：

```go
func (ga *GovernanceAgent) ValidateCode(language string, code []byte, caps CapabilitySet) error {
    switch language {
    case "python":
        return validateByPatterns(code, pythonDangerousPatterns, caps)
    case "bash", "sh":
        return validateByPatterns(code, bashDangerousPatterns, caps)
    case "wasm":
        return ga.ValidateWasmImports(code, caps)
    default:
        // 未知语言：记录日志，不拦截（不能假设恶意）
        slog.Warn("code_validator: unknown language, skipping", "language", language)
        return nil
    }
}
```

---

### 任务 9：新增 Wasm Import Section 白名单校验

新建 `pkg/swarm/agents/wasm_validator.go`（或合并入 `code_validator.go`）：

```go
// ValidateWasmImports 解析 Wasm 二进制的 Import Section，
// 拒绝导入了白名单之外宿主函数的 Wasm 模块。
//
// 设计原则：
//   确定性检查，与代码逻辑内容无关（不受混淆影响）。
//   只解析 Import Section（模块头部），不需要完整反编译。
//   O(import_count)，微秒级完成。
//
// Wasm 二进制格式简介（用于实现参考）：
//   字节 0-3：魔数 \0asm
//   字节 4-7：版本 \x01\x00\x00\x00
//   之后是若干 Section，每个 Section 格式：[section_id u8][size uleb128][content...]
//   Import Section 的 section_id = 2
//   Import Section 内每条 import：[module_name str][field_name str][kind u8][...]
//
// 实现说明：
//   推荐使用 Go 的 `golang.org/x/tools/...` 或手动解析 LEB128 编码。
//   也可以直接依赖 `github.com/tetratelabs/wazero/api`（Wazero 已是 Polaris 依赖）
//   用 wazero 的 wasm.DecodeModule 函数直接拿到 ImportSection，最简洁。
//
// 返回：nil = 通过，error = 包含具体违规 import 名称
func (ga *GovernanceAgent) ValidateWasmImports(wasmBytes []byte, caps CapabilitySet) error
```

**WASI 函数白名单定义**：

```go
// wasiAllowedImports 始终允许的 WASI 导入（无需能力）。
var wasiAllowedImports = map[string]bool{
    "wasi_snapshot_preview1:fd_write":        true, // stdout/stderr 输出
    "wasi_snapshot_preview1:fd_read":         true, // stdin 读取
    "wasi_snapshot_preview1:fd_close":        true,
    "wasi_snapshot_preview1:fd_seek":         true,
    "wasi_snapshot_preview1:fd_fdstat_get":   true,
    "wasi_snapshot_preview1:args_get":        true, // 命令行参数
    "wasi_snapshot_preview1:args_sizes_get":  true,
    "wasi_snapshot_preview1:environ_get":     true, // 环境变量读取
    "wasi_snapshot_preview1:environ_sizes_get": true,
    "wasi_snapshot_preview1:clock_time_get":  true, // 时钟
    "wasi_snapshot_preview1:proc_exit":       true, // 正常退出
    "wasi_snapshot_preview1:random_get":      true, // 随机数
    // VFS 路径（受沙箱 preopens 限制，物理安全）
    "wasi_snapshot_preview1:path_open":       true,
    "wasi_snapshot_preview1:path_read_link":  true,
    "wasi_snapshot_preview1:fd_readdir":      true,
    "wasi_snapshot_preview1:fd_prestat_get":  true,
    "wasi_snapshot_preview1:fd_prestat_dir_name": true,
}

// wasiCapabilityGatedImports 需要特定能力才允许的 WASI 导入。
var wasiCapabilityGatedImports = map[string]string{
    // key = "module:field"，value = 所需能力名称
    "wasi_snapshot_preview1:sock_open":          "network_raw",
    "wasi_snapshot_preview1:sock_connect":       "network_raw",
    "wasi_snapshot_preview1:sock_send":          "network_raw",
    "wasi_snapshot_preview1:sock_recv":          "network_raw",
    "wasi_snapshot_preview1:sock_accept":        "network_listen",
    "wasi_snapshot_preview1:path_remove_directory": "destructive_fs",
    "wasi_snapshot_preview1:path_unlink_file":   "destructive_fs",
    "wasi_snapshot_preview1:path_rename":        "destructive_fs",
    "wasi_snapshot_preview1:path_create_directory": "fs_write",
    "wasi_snapshot_preview1:proc_raise":         "process_signal",
    // 非 WASI 的宿主函数（自定义导入）全部拒绝，除非有 "custom_host_fn" 能力
}
```

**实现逻辑**：

```go
// ValidateWasmImports 实现：
// 1. 校验魔数和版本头（\0asm\x01\x00\x00\x00），失败返回错误（不是合法 Wasm）
// 2. 遍历所有 Section，找到 section_id = 2 的 Import Section
// 3. 解析每条 import：module + field 拼成 key = "module:field"
// 4. 检查 key 是否在 wasiAllowedImports → 允许，继续
// 5. 检查 key 是否在 wasiCapabilityGatedImports → 检查 caps 是否有对应能力
//    有能力 → 允许，无能力 → 返回错误
// 6. 既不在白名单也不在门控表 → 拒绝（未知宿主函数，保守策略）
//
// 推荐使用 wazero 的 wasm 解析：
//   import "github.com/tetratelabs/wazero/internal/wasm"
//   m, err := wasm.DecodeModule(wasmBytes, wasm.Features20220419, wasm.MemoryLimitPages(16), false, false, false, false)
//   for _, imp := range m.ImportSection { ... }
//
// 若 wazero 内部包不可访问，手动解析 LEB128：
//   提供 readU32LEB128(data []byte, offset int) (val uint32, newOffset int) 辅助函数
```

---

### 任务 9 的接入点

`ValidateCode` 和 `ValidateWasmImports` 需要接入代码执行路径。

在 `pkg/action/code_act.go` 的 `Execute()` 方法中，在写临时文件之前，增加调用：

```go
// 代码级安全检查（确定性规则，非 LLM）
// govAgent 由 CodeAct 的调用方注入（可为 nil，nil 时跳过检查）
if a.govAgent != nil {
    caps := extractCapabilities(req.CapabilityToken) // 从 token 解析能力集
    if err := a.govAgent.ValidateCode(req.Language, []byte(req.Code), caps); err != nil {
        return nil, perrors.Wrap(perrors.CodeForbidden, "code validation rejected", err)
    }
}
```

`CodeAct` 结构体新增字段：
```go
govAgent interface {
    ValidateCode(language string, code []byte, caps CapabilitySet) error
}
```

提供 `WithGovernanceAgent(ga *GovernanceAgent) CodeActOption` 选项函数，允许在初始化时注入。

---

## 四、验收标准

```bash
make fmt      # 无输出
make lint     # 无 error / warning
make test     # 全部 PASS
make build    # 二进制构建成功
```

### 新功能逐项检查

- [ ] `memory_agent.go`：`distill()` 方法已实现，`Run()` 已解注释，`make test` 有对应单测
- [ ] `governance_agent.go`：`probeMemory()` 有实际内存读取，Linux 和 macOS 均有实现
- [ ] `governance_agent.go`：`CheckIdempotent()` 有 SQL 查询，`RecordExecution()` 有 SQL 写入
- [ ] `extension_librarian_handler.go`：`Handle()` 有 LLM 调用 + SurrealDB 写入
- [ ] `semantic_compress_handler.go`：`Handle()` 有 LLM 调用 + VFS 文件回写
- [ ] `reflexion.go`：`replaySuccess()` goroutine 内有 SurrealDB 写入 + reflection_memory 写入
- [ ] `planner/pool.go`：`taskType="code_act"` 时走 Engine A（编译+单测打分）
- [ ] `mock_proxy.go`：重写为 HTTP 代理服务器，有 `ServeHTTP` + `EnvVars()`
- [ ] `sandbox_impl.go`：`DryRunMode=true` 时创建 MockProxy 并注入环境变量
- [ ] `code_validator.go`（或 governance_agent.go）：`ValidateCode()` 实现 Python + Bash 规则扫描
- [ ] `wasm_validator.go`（或同文件）：`ValidateWasmImports()` 解析 Import Section + 白名单校验
- [ ] `code_act.go`：执行前调用 `govAgent.ValidateCode()`

### 架构约束检查

- [ ] 所有新增代码无全局可变变量
- [ ] 所有 LLM 调用有 30s context timeout
- [ ] 所有 SQL 操作使用 `internal/errors` 包装错误
- [ ] `ValidateCode` 和 `ValidateWasmImports` 无 LLM 调用（确定性，微秒级）

---

## 五、禁止事项

- **禁止**修改 `pkg/cognition/fsm.go` 任何现有逻辑
- **禁止**修改 `internal/protocol/schema/` 任何已有 SQL 文件
- **禁止**在安全检查路径（`ValidateCode` / `ValidateWasmImports`）引入 LLM 调用
- **禁止**将 AST 安全扫描结果作为唯一安全边界——它是辅助层，沙箱是最终防线
- **禁止**顺手重构本次任务范围之外的代码
