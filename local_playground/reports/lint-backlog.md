<!-- 由 make review-merge 生成；状态列由修复轮维护（见 prompt/fix.md）-->
# 可机械化待办

> 修复轮纪律：本表 pending 清零（landed 或带理由 rejected）之后，才允许开始修 GR 条目。
> 规则先行的理由：已落地的规则能自动验证 GR 修复的完备性；反序则规则永远排在「更紧急的修复」之后，无限延期。

| 来源ID | 问题类别 | 建议规则 | 状态(pending/landed/rejected) |
|---|---|---|---|
| GR-10-002 | HE 不变量违反（B） | AST 检查 scoreShadow 头部 if e.llmProvider == nil 返回 true, nil | landed (规则已就绪，见 lint-selftest.txt，commit 3cef8d5) |
| GR-10-004 | HE 不变量违反（B） | grep "smtp.SendMail(" | landed (规则已就绪，见 lint-selftest.txt，commit 3cef8d5) |
| GR-6-001 | HE 不变量违反（B） | StageEphemeralFile 中传入 filename 必须经过 filepath.Base / resolveWithinRoot 校验，禁止直拼 filepath.Join | landed (规则已就绪，见 lint-selftest.txt，commit 3cef8d5) |
| GR-7-001 | Taint 传播断点（D） | KnowledgeBase.Search 结果组合前必须校验 chunk.TaintLevel <= req.TaintMax，超出必须过滤 | landed (规则已就绪，见 lint-selftest.txt，commit 3cef8d5) |
| GR-9-002 | HE 不变量违反（B） | 扫描 AST 中 os.ReadFile 前的 filepath 校验逻辑，确保存在 strings.HasPrefix(abs, cleanWorkDir) 越界断言 | landed (规则已就绪，见 lint-selftest.txt，commit 3cef8d5) |
| GR-1-001 | 并发与资源（C） | ast 检查 dw.ResultCh <- expr 未包裹在 select-default 块中 | landed (规则已就绪，见 lint-selftest.txt，commit 3cef8d5) |
| GR-1-002 | 幂等重放与一致性（M） | 检查 CrashRecoveryCount >= 3 分支返回值是否为非 nil 错误 | landed (规则已就绪，见 lint-selftest.txt，commit 3cef8d5) |
| GR-10-001 | 幂等重放与一致性（M） | grep "st.Status == \"completed\" || st.Status == \"failed\"" 检查包含 running 状态过滤 | landed (规则已就绪，见 lint-selftest.txt，commit 3cef8d5) |
| GR-11-001 | HE 不变量违反（B） | grep -rn 'CStr::from_ptr(' rust/substrate/src/ | grep -v 'is_null()' | landed (规则已就绪，见 lint-selftest.txt，commit 3cef8d5) |
| GR-3-001 | docs↔code 漂移（A/K） | 检查规范文档中模块依赖声明与 cmd/polaris/ 实际 import 导入集的求差校验 | landed (规则已就绪，见 lint-selftest.txt，commit 3cef8d5) |
| GR-3-002 | 生命周期与关停（L） | AST 检查 Bootstrapper 关停 Phase 循环中对 b.modules map 的直接 range 遍历 | rejected (由单测覆盖，见 WP-5) |
| GR-4-001 | LLM/Agent 生产陷阱（H） | grep `"with open(__CA_STATE_FILE__, \"wb\")"` 检索并在 AST/正则上校验是否位于 try-except 外部 | landed (规则已就绪，见 lint-selftest.txt，commit 3cef8d5) |
| GR-4-002 | Taint 传播断点（D） | grep `WriteUserData\(taint\.NewTaintedString\(.*ExecuteResult.*TaintMedium` 断言此处错误硬编码 | landed (规则已就绪，见 lint-selftest.txt，commit 3cef8d5) |
| GR-5-004 | 错误处理与边界（E） | AST check for switch cases on classifier.RiskHITL missing HITL prompt or return | landed (规则已就绪，见 lint-selftest.txt，commit 3cef8d5) |
| GR-6-002 | 并发与资源（C） | 状态图构建 requiredPreds 时须剔除可达环路中的回边，不能仅过滤自环 From != To | rejected (在 ValidateStateGraphTopology 运行期校验，见 WP-6) |
| GR-7-002 | 生命周期与关停（L） | Engine.Start 中 select-case 读取通道时，!ok 应对通道置 nil 继续循环，禁止直接 return nil | landed (规则已就绪，见 lint-selftest.txt，commit 3cef8d5) |
| GR-8-001 | 错误处理与边界（E） | `grep -n "!.*\.IsSafe()" internal/extension/marketplace/marketplace.go` | landed (规则已就绪，见 lint-selftest.txt，commit 3cef8d5) |
| GR-8-002 | LLM/Agent 生产陷阱（H） | `grep -n "regexp.MustCompile.*(?s)" internal/extension/` | landed (规则已就绪，见 lint-selftest.txt，commit 3cef8d5) |
| GR-9-001 | HE 不变量违反（B） | 检查 handleAgentInterrupt 中 authCtx.ClientType 的判断，比对 middleware_auth.go 注入的 WebUI 客户端类型 webui | landed (规则已就绪，见 lint-selftest.txt，commit 3cef8d5) |
| GR-1-003 | LLM/Agent 生产陷阱（H） | AST 检查 probe.TierParameters 结构体中是否存在废弃字段 GraphRAGLLMDailyBudget | landed (规则已就绪，见 lint-selftest.txt，commit 3cef8d5) |
| GR-1-004 | 其他 | 检查 downloadChunk 中 os.O_TRUNC 标志设置时是否清空 offset 或删除旧 part 文件 | landed (规则已就绪，见 lint-selftest.txt，commit 3cef8d5) |
| GR-10-003 | 接线断裂（G-bis） | 导出符号生产调用方可达性扫描，测试调用不计 | landed (规则已就绪，见 lint-selftest.txt，commit 3cef8d5) |
| GR-12-001 | Schema/配置漂移（F） | grep -n "028_apps" docs/arch/M02-Storage-Fabric.md 提取并核对 DDL 文件总数及上限 | landed (规则已就绪，见 lint-selftest.txt，commit 3cef8d5) |
| GR-12-002 | docs↔code 漂移（A/K） | grep -n "AgentState:" docs/arch/M04-Agent-Kernel.md 校验状态枚举列表完整性与权威定义文件 | landed (规则已就绪，见 lint-selftest.txt，commit 3cef8d5) |
| GR-12-003 | docs↔code 漂移（A/K） | 校验 docs/arch/INDEX.md §1 表中 est_tok 估算值与实际文件 Byte/Token 的比例漂移 | rejected (由 make docs-gen-check 兜底) |
| GR-12-004 | docs↔code 漂移（A/K） | grep -n "Code.*Code = " pkg/apperr/apperr.go 与 docs/specs/00-Constitution.md §R2.5 错误码列表逐一求差集 | landed (规则已就绪，见 lint-selftest.txt，commit 3cef8d5) |
| GR-12-005 | Schema/配置漂移（F） | grep -n "001-006_\*\.sql" docs/arch/00-Global-Dictionary.md 提取并校验 DDL 范围 | landed (规则已就绪，见 lint-selftest.txt，commit 3cef8d5) |
| GR-12-006 | docs↔code 漂移（A/K） | 提取 docs/arch/M02-Storage-Fabric.md §2 tasks 表列定义与 internal/protocol/schema/007_tasks.sql 列定义求差集 | landed (规则已就绪，见 lint-selftest.txt，commit 3cef8d5) |
| GR-12-007 | Schema/配置漂移（F） | grep -n "'official'" docs/arch/M13-bis-Extension-Registry.md 校验 origin 枚举列表内部一致性 | landed (规则已就绪，见 lint-selftest.txt，commit 3cef8d5) |
| GR-2-003 | 接线断裂（G-bis） | grep -rn "security\.AuditRepo" internal/ 且测试不计入 | landed (规则已就绪，见 lint-selftest.txt，commit 3cef8d5) |
| GR-3-003 | 其他 | AST 检查 Bootstrapper.Ignite 循环中 mod.Init 失败处理缺失逆序 Cleanup 逻辑 | rejected (由单测覆盖，见 WP-5) |
| GR-5-001 | 接线断裂（G-bis） | 导出函数生产调用方可达性扫描，测试调用不计 | landed (规则已就绪，见 lint-selftest.txt，commit 3cef8d5) |
| GR-5-002 | 接线断裂（G-bis） | 导出函数生产调用方可达性扫描，测试调用不计 | landed (规则已就绪，见 lint-selftest.txt，commit 3cef8d5) |
| GR-5-003 | 接线断裂（G-bis） | 导出函数生产调用方可达性扫描，测试调用不计 | landed (规则已就绪，见 lint-selftest.txt，commit 3cef8d5) |
| GR-5-005 | 接线断裂（G-bis） | 导出函数生产调用方可达性扫描，测试调用不计 | landed (规则已就绪，见 lint-selftest.txt，commit 3cef8d5) |
| GR-6-003 | 其他 | 仅含 package 声明且无任何导出的源文件应清除 | rejected (一次性处置，见 WP-10) |
| GR-7-003 | HE 不变量违反（B） | 在 context.WithTimeout 派生后执行沙箱/子进程操作前必须检查 parent ctx.Err | landed (规则已就绪，见 lint-selftest.txt，commit 3cef8d5) |
| GR-8-003 | 其他 | `grep -n "rate limit exceeded" internal/extension/skill/skill_executor.go` | landed (规则已就绪，见 lint-selftest.txt，commit 3cef8d5) |
| GR-8-004 | Taint 传播断点（D） | `grep -n "makeMCPToolAsyncFn" internal/extension/mcp/mcp_manager_tools.go` | rejected (该条目本身是误报，见 §2) |
| GR-8-005 | 其他 | `grep -n "scanner.Buffer" internal/extension/mcp/mcp_client_stdio.go` | landed (规则已就绪，见 lint-selftest.txt，commit 3cef8d5) |
| GR-9-003 | HE 不变量违反（B） | 提取 mux.HandleFunc 模式中的路径参数与 Handler 中 r.PathValue 调用的 key 集合，校验子集包含关系 | landed (规则已就绪，见 lint-selftest.txt，commit 3cef8d5) |
