# 代码轨道 批次 8 审核报告（L2-扩展生态）

| ID | 严重级/动作 | 模块或对象 | 一句话标题 | 置信度 | 可机械化 |
|---|---|---|---|---|---|
| GR-8-001 | P1 | internal/extension/marketplace | NewMCPMarketplaceClient 中的 SafeHTTPClient 校验缺乏 nil 空值防护导致 panic 崩溃 | 高 | 是 |
| GR-8-002 | P1 | internal/extension/skill | skill_creator 与 plugin_creator 的 extractJSON 贪婪正则 (?s)\{.*\} 导致多括号 LLM 响应解析失败 | 高 | 是 |
| GR-8-003 | P2 | internal/extension/skill | ScriptSkillExecutor 限流触发时错误码误用 CodeInternal 替代 CodeResourceExhausted | 高 | 是 |
| GR-8-004 | P2 | internal/extension/mcp | makeMCPToolAsyncFn 异步变体工具返回结果未标注 TaintLevel 退化为 TaintNone | 高 | 是 |
| GR-8-005 | P2 | internal/extension/mcp | stdio readLoop 的 bufio.Scanner 硬编码 1MB 缓冲区上限导致大载荷 MCP 响应断连 | 高 | 是 |

置信度分布声明: 本批次 5 条发现均已通过源码精读与 §2-A 四处反证核对（boot_*.go / bootstrap / 注册表 / 反射），判定均为证据行直接可见的确定性问题。

### [GR-8-001] NewMCPMarketplaceClient 中的 SafeHTTPClient 校验缺乏 nil 空值防护导致 panic 崩溃
- 严重级: P1
- 模块: internal/extension/marketplace（层: L2）
- 位置: internal/extension/marketplace/marketplace.go:40
- 违反规则: R1.14 | HE-2 | 维度D
- 置信度: 高
- 可机械化: 是（建议规则: `grep -n "!.*\.IsSafe()" internal/extension/marketplace/marketplace.go`）
- 反证: 已查 cmd/polaris/boot_*.go, internal/bootstrap/, 注册表, 反射四处。boot_tools.go 中生产装配路径传入了 SafeHTTP 实例；但 NewMCPMarketplaceClient 的文档注释明文声称「传 nil 时降级为裸 http.Client（仅测试场景允许）」，而实现第 40 行无条件调用 `!httpClient.IsSafe()`，当未注入 httpClient 传入 nil 时会触发 Go `nil pointer dereference` 运行时 panic 崩溃，未能按 R1.14 要求做到安全的 fail-closed 判断。
- 问题: NewMCPMarketplaceClient 构造函数在文档注释中声明支持传入 nil httpClient 作为降级路径，但第 40 行代码未做 nil 解引用判空直接调用 `!httpClient.IsSafe()`。当外部测试或可选调用方传入 nil 时程序直接 panic 崩溃。
- 证据:
  ```go
  	if !httpClient.IsSafe() {
  		panic("marketplace: httpClient must be a valid network.SafeHTTPClient")
  	}
  ```
- 修复方向提示: 在调用 IsSafe 前增加 nil 判定，若 nil 则返回 fail-closed error 或安全初始化 SafeHTTPClient。

### [GR-8-002] skill_creator 与 plugin_creator 的 extractJSON 贪婪正则 (?s)\{.*\} 导致多括号 LLM 响应解析失败
- 严重级: P1
- 模块: internal/extension/skill（层: L2）
- 位置: internal/extension/skill/skill_creator.go:234
- 违反规则: A-01 | P-2 | 维度H
- 置信度: 高
- 可机械化: 是（建议规则: `grep -n "regexp.MustCompile.*(?s)" internal/extension/`）
- 反证: 已查 boot_*.go / bootstrap / 注册表 / 反射，生成 Skill/Plugin 的请求经由 sysadmin HTTP Handler 调用 `GenerateSkill`/`GeneratePlugin`。当 LLM 响应包含 Markdown 提示词或正文前后含有 `{}`（如 `Here is option {1}: \n{...}\n Note: use {option 2}`）时，`(?s)\{.*\}` 的贪婪匹配从最外层第一个 `{` 截断至最后一个 `}`，使得截取的 JSON 文本包含非 JSON 前后缀，导致 `json.Unmarshal` 100% 失败并耗尽重试。
- 问题: `skill_creator.go` 与 `plugin_creator.go` 中的 `extractJSON` 函数均使用了 `(?s)\{.*\}` 贪婪匹配正则表达式。当 LLM 输出包含多个 JSON 块或在 JSON 前后文本中包含花括号 `{}` 时，正则表达式会跨非 JSON 文本大范围贪婪截取，导致得到的并非合法的 JSON 对象，破坏了 A-01 / P-2 的 LLM 响应格式兜底机制。
- 证据:
  ```go
  var jsonExtractRegex = regexp.MustCompile(`(?s)\{.*\}`)

  func extractJSON(input string) string {
  	match := jsonExtractRegex.FindString(input)
  ```
- 修复方向提示: 改用非贪婪匹配或基于括号计数 / json.RawMessage 边界提取的非贪婪 JSON 对象截取算法。

### [GR-8-003] ScriptSkillExecutor 限流触发时错误码误用 CodeInternal 替代 CodeResourceExhausted
- 严重级: P2
- 模块: internal/extension/skill（层: L2）
- 位置: internal/extension/skill/skill_executor.go:85
- 违反规则: P-9 | R2.5 | 维度H
- 置信度: 高
- 可机械化: 是（建议规则: `grep -n "rate limit exceeded" internal/extension/skill/skill_executor.go`）
- 反证: 已查 cmd/polaris/boot_*.go, internal/bootstrap/, 注册表, 反射四处。`ScriptSkillExecutor.ExecuteSkill` 被 Agent 执行引擎在 S_EXECUTE 阶段调用。限流触发时返回的错误码被指定为 `apperr.CodeInternal`（对应 HTTP 500），而上层重试与熔断逻辑依赖 `apperr.CodeOf(err)` 识别限流/资源耗尽（`CodeResourceExhausted` 429）以进行退避重试，误用 CodeInternal 会导致上层误判为系统崩溃事故。
- 问题: `ScriptSkillExecutor` 在技能执行超过限流阈值 (20 QPS) 时，使用 `apperr.CodeInternal` 构造错误并抛出。违反了 P-9 全链路错误语义化与 `pkg/apperr` 错误码规范映射要求，阻断了上层对 429 资源限流错误的精准感知。
- 证据:
  ```go
  	// P1-8 限流：Skill 执行速率上限 20 QPS
  	if !e.skillLimiter.Allow() {
  		return nil, apperr.New(apperr.CodeInternal, fmt.Sprintf("skill_executor: rate limit exceeded for skill %s", skillID))
  	}
  ```
- 修复方向提示: 将 `apperr.CodeInternal` 替换为 `apperr.CodeResourceExhausted`。

### [GR-8-004] makeMCPToolAsyncFn 异步变体工具返回结果未标注 TaintLevel 退化为 TaintNone
- 严重级: P2
- 模块: internal/extension/mcp（层: L2）
- 位置: internal/extension/mcp/mcp_manager_tools.go:211
- 违反规则: HE-2 | 维度D | 维度M
- 置信度: 高
- 可机械化: 是（建议规则: `grep -n "makeMCPToolAsyncFn" internal/extension/mcp/mcp_manager_tools.go`）
- 反证: 已查 cmd/polaris/boot_*.go, internal/bootstrap/, 注册表, 反射四处。MCP 异步工具变体在注册时由 `makeMCPToolAsyncFn` 构造 ToolResult，返回 `&types.ToolResult{Success: true, Output: out}`，未设置 `TaintLevel` 字段，默认值为 Go 零值 0 (`TaintNone`)。尽管该返回结果为 `task_id` 包装载荷，但外部 MCP Server 工具的交互结果应继承该 Server 对应的静态污点策略（`TaintMedium` 或 `TaintHigh`），直接返回 `TaintNone` 导致后续 DAG 执行节点的污点追踪被截断。
- 问题: `makeMCPToolAsyncFn` 函数中构造异步变体响应 `ToolResult` 时，漏掉了 `TaintLevel` 字段赋值。这使得从外部/未受信 MCP 工具派生的异步任务 Initial ToolResult 在进入系统后被误标为 `TaintNone` (系统级无污点)，违反了 HE-2 可验证执行中污点只升不降的传播规范。
- 证据:
  ```go
  		taskID := m.runAsyncCall(ctx, client, mcpName, args)
  		out, _ := json.Marshal(map[string]string{"task_id": taskID, "status": string(AsyncTaskPending)}) //nolint:errchkjson // 固定字段结构体，Marshal 不会失败
  		return &types.ToolResult{Success: true, Output: out}, nil
  ```
- 修复方向提示: 构造 ToolResult 时显式传入该 MCP 工具注册时的静态 `taint` 等级。

### [GR-8-005] stdio readLoop 的 bufio.Scanner 硬编码 1MB 缓冲区上限导致大载荷 MCP 响应断连
- 严重级: P2
- 模块: internal/extension/mcp（层: L2）
- 位置: internal/extension/mcp/mcp_client_stdio.go:72
- 违反规则: Tier-0-Limit | 维度D | 维度H
- 置信度: 高
- 可机械化: 是（建议规则: `grep -n "scanner.Buffer" internal/extension/mcp/mcp_client_stdio.go`）
- 反证: 已查 cmd/polaris/boot_*.go, internal/bootstrap/, 注册表, 反射四处。stdio 模式下 MCP 客户端建立子进程后由 `readLoop` 循环按行读取 stdout 的 JSON-RPC 消息。`scanner.Buffer` 的最大容量被写死为 `1024*1024` (1MB)。当 MCP 工具返回大型 Base64 图片数据、大型文件或长上下文知识块时，单行 JSON 超过 1MB 会导致 `scanner.Scan()` 触发 `bufio.ErrTooLong` 并退出循环、自动调用 `c.Close()` 导致 MCP 连接意外断开崩溃。
- 问题: stdio 传输层中的 `readLoop` 为 `bufio.Scanner` 设置了固定 1MB 的 `maxBuffer` 上限。在包含多模态图片 (image content block) 或大图谱数据的场景中，单次 JSON-RPC 消息体很容易超过 1MB，引发 `token too long` 扫描错误，强制关停底层进程管道。
- 证据: internal/extension/mcp/mcp_client_stdio.go:93-98
  ```go
  func (c *MCPClient) readLoop(r io.Reader) {
  	scanner := bufio.NewScanner(r)
  	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
  	for scanner.Scan() {
  ```
- 修复方向提示: 增大 maxBuffer 缓冲区上限（例如 16MB 或 64MB），或改用 json.Decoder 块读取流式解析。

## 已审文件清单
1. `internal/extension/lifecycle/app_installer.go`
2. `internal/extension/lifecycle/fsm.go`
3. `internal/extension/lifecycle/installer.go`
4. `internal/extension/lifecycle/mcp_installer.go`
5. `internal/extension/lifecycle/plugin_installer.go`
6. `internal/extension/lifecycle/skill_installer.go`
7. `internal/extension/llmgen/generate_structured.go`
8. `internal/extension/marketplace/adapter.go`
9. `internal/extension/marketplace/adapter_heuristic.go`
10. `internal/extension/marketplace/manager.go`
11. `internal/extension/marketplace/marketplace.go`
12. `internal/extension/mcp/agent_descriptor.go`
13. `internal/extension/mcp/async_tasks.go`
14. `internal/extension/mcp/env.go`
15. `internal/extension/mcp/mcp_client.go`
16. `internal/extension/mcp/mcp_client_http.go`
17. `internal/extension/mcp/mcp_client_protocol.go`
18. `internal/extension/mcp/mcp_client_rpc.go`
19. `internal/extension/mcp/mcp_client_serverreq.go`
20. `internal/extension/mcp/mcp_client_sse.go`
21. `internal/extension/mcp/mcp_client_stdio.go`
22. `internal/extension/mcp/mcp_manager.go`
23. `internal/extension/mcp/mcp_manager_lifecycle.go`
24. `internal/extension/mcp/mcp_manager_net.go`
25. `internal/extension/mcp/mcp_manager_tools.go`
26. `internal/extension/mcp/mcp_registry.go`
27. `internal/extension/mcp/taint_decoder.go`
28. `internal/extension/mcp/tool_scanner.go`
29. `internal/extension/models/doc.go`
30. `internal/extension/native/extension_activator.go`
31. `internal/extension/native/extension_manager.go`
32. `internal/extension/native/extension_tools.go`
33. `internal/extension/native/loader.go`
34. `internal/extension/plugin/plugin_creator.go`
35. `internal/extension/plugin/plugin_creator_llm_adapter.go`
36. `internal/extension/provider.go`
37. `internal/extension/skill/compile.go`
38. `internal/extension/skill/generate.go`
39. `internal/extension/skill/matcher.go`
40. `internal/extension/skill/retriever.go`
41. `internal/extension/skill/script_skill_cache.go`
42. `internal/extension/skill/sdk/sdk.go`
43. `internal/extension/skill/skill.go`
44. `internal/extension/skill/skill_creator.go`
45. `internal/extension/skill/skill_creator_llm_adapter.go`
46. `internal/extension/skill/skill_evolution.go`
47. `internal/extension/skill/skill_executor.go`
48. `internal/extension/skill/skill_middleware.go`
49. `internal/extension/skill/skill_pipeline.go`
50. `internal/extension/skill/skill_types.go`
51. `internal/extension/skill/sqlite_registry.go`

## 明确未覆盖的范围
无

## 审了但无发现的模块
- `internal/extension/llmgen`
- `internal/extension/models`
