<!-- 由 make review-merge 生成；状态列由修复轮维护（见 prompt/fix.md）-->
# 可机械化待办

> 修复轮纪律：本表 pending 清零（landed 或带理由 rejected）之后，才允许开始修 GR 条目。
> 规则先行的理由：已落地的规则能自动验证 GR 修复的完备性；反序则规则永远排在「更紧急的修复」之后，无限延期。

| 来源ID | 问题类别 | 建议规则 | 状态(pending/landed/rejected) | 说明 |
|---|---|---|---|---|
| GR-10-002 | HE 不变量违反（B） | AST 检查 scoreShadow 头部 if e.llmProvider == nil 返回 true, nil | rejected | 判据无法泛化：「nil 依赖时返回放行值」需要知道每个函数的哪个返回值代表放行，AST 上不可判。缺陷本身已按 fail-closed 修复并有单测；2026-08-13 复核发现原记录的 landed 落点 L-04 与 panic-check 跑同一二进制、零新增覆盖，已删除该目标 |
| GR-10-004 | HE 不变量违反（B） | grep "smtp.SendMail(" | landed | 规则已就绪，见 lint-selftest.txt，commit 3cef8d5 |
| GR-6-001 | HE 不变量违反（B） | StageEphemeralFile 中传入 filename 必须经过 filepath.Base / resolveWithinRoot 校验，禁止直拼 filepath.Join | landed | 规则已就绪，见 lint-selftest.txt，commit 3cef8d5 |
| GR-7-001 | Taint 传播断点（D） | KnowledgeBase.Search 结果组合前必须校验 chunk.TaintLevel <= req.TaintMax，超出必须过滤 | landed | 规则已就绪，见 lint-selftest.txt，commit 3cef8d5 |
| GR-9-002 | HE 不变量违反（B） | 扫描 AST 中 os.ReadFile 前的 filepath 校验逻辑，确保存在 strings.HasPrefix(abs, cleanWorkDir) 越界断言 | rejected | os.ReadFile 前必须有根目录断言这条判据假阳性面过大——配置/嵌入资源/日志轮转等合法读取点全中，白名单会立刻膨胀成第二个存量表。缺陷本身已修并有 5 个用例覆盖越界/符号链接/绝对路径 |
| GR-1-001 | 并发与资源（C） | ast 检查 dw.ResultCh <- expr 未包裹在 select-default 块中 | landed | 规则已就绪，见 lint-selftest.txt，commit 3cef8d5 |
| GR-1-002 | 幂等重放与一致性（M） | 检查 CrashRecoveryCount >= 3 分支返回值是否为非 nil 错误 | landed | 规则已就绪，见 lint-selftest.txt，commit 3cef8d5 |
| GR-10-001 | 幂等重放与一致性（M） | AST 断言 scanAndDispatch 的状态过滤分支存在 "running" 判定，或存在 in-flight 集合成员判定 | landed | L-07 tools/scheduler_status_filter_check.go；负向用例见 lint-selftest.txt L-07 行 |
| GR-11-001 | HE 不变量违反（B） | 遍历 rust 全部 extern "C" 导出函数，对每个 `*const` 入参断言存在 is_null 判定或经 null-safe 助手取值（原建议的 `grep -v is_null` 写法会漏 slice::from_raw_parts 且对 slice_to_str 全假阳性） | landed | L-08 tools/ffi_null_guard_check.go；2026-08-13 复核收窄判据：只查入参、出参交由 write_* 助手，避免把文档化的可空出参打成硬失败 |
| GR-3-001 | docs↔code 漂移（A/K） | 检查规范文档中模块依赖声明与 cmd/polaris/ 实际 import 导入集的求差校验 | landed | 规则已就绪，见 lint-selftest.txt，commit 3cef8d5 |
| GR-3-002 | 生命周期与关停（L） | AST 检查 Bootstrapper 关停 Phase 循环中对 b.modules map 的直接 range 遍历 | rejected | 由单测覆盖，见 WP-5 |
| GR-4-001 | LLM/Agent 生产陷阱（H） | grep `"with open(__CA_STATE_FILE__, \"wb\")"` 检索并在 AST/正则上校验是否位于 try-except 外部 | landed | 规则已就绪，见 lint-selftest.txt，commit 3cef8d5 |
| GR-4-002 | Taint 传播断点（D） | grep `WriteUserData\(taint\.NewTaintedString\(.*ExecuteResult.*TaintMedium` 断言此处错误硬编码 | landed | 规则已就绪，见 lint-selftest.txt，commit 3cef8d5 |
| GR-5-004 | 错误处理与边界（E） | AST check for switch cases on classifier.RiskHITL missing HITL prompt or return | rejected | 判据依赖"审批调用"的语义识别，AST 上只能匹配特定函数名，换个包装即失效。缺陷本身已修：两个工具的 RiskHITL 分支改为网关缺失即拒 |
| GR-6-002 | 并发与资源（C） | 状态图构建 requiredPreds 时须剔除可达环路中的回边，不能仅过滤自环 From != To | rejected | 在 ValidateStateGraphTopology 运行期校验，见 WP-6 |
| GR-7-002 | 生命周期与关停（L） | Engine.Start 中 select-case 读取通道时，!ok 应对通道置 nil 继续循环，禁止直接 return nil | landed | 规则已就绪，见 lint-selftest.txt，commit 3cef8d5 |
| GR-8-001 | 错误处理与边界（E） | `grep -n "!.*\.IsSafe()" internal/extension/marketplace/marketplace.go` | landed | 并入 tools/panic_lint.go：导出构造函数内 panic 单独报错文案；负向用例见 lint-selftest.txt F-12 行 |
| GR-8-002 | LLM/Agent 生产陷阱（H） | `grep -n "regexp.MustCompile.*(?s)" internal/extension/` | landed | 规则已就绪，见 lint-selftest.txt，commit 3cef8d5 |
| GR-9-001 | HE 不变量违反（B） | 检查 handleAgentInterrupt 中 authCtx.ClientType 的判断，比对 middleware_auth.go 注入的 WebUI 客户端类型 webui | landed | 规则已就绪，见 lint-selftest.txt，commit 3cef8d5 |
| GR-1-003 | LLM/Agent 生产陷阱（H） | AST 检查 probe.TierParameters 结构体中是否存在废弃字段 GraphRAGLLMDailyBudget | landed | 规则已就绪，见 lint-selftest.txt，commit 3cef8d5 |
| GR-1-004 | 其他 | 检查 downloadChunk 中 os.O_TRUNC 标志设置时是否清空 offset 或删除旧 part 文件 | landed | 规则已就绪，见 lint-selftest.txt，commit 3cef8d5 |
| GR-10-003 | 接线断裂（G-bis） | 导出符号生产调用方可达性扫描，测试调用不计 | landed | L-13 tools/wiring_reachability_check.go，包级判据；2026-08-13 重写为 go list 闭包比对——初版跑 deadcode 需联网、与 make deadcode 完全重复，且正则匹配不到方法，而本类条目全是方法，结构上抓不到 |
| GR-12-001 | Schema/配置漂移（F） | grep -n "028_apps" docs/arch/M02-Storage-Fabric.md 提取并核对 DDL 文件总数及上限 | landed | 规则已就绪，见 lint-selftest.txt，commit 3cef8d5 |
| GR-12-002 | docs↔code 漂移（A/K） | grep -n "AgentState:" docs/arch/M04-Agent-Kernel.md 校验状态枚举列表完整性与权威定义文件 | landed | 规则已就绪，见 lint-selftest.txt，commit 3cef8d5 |
| GR-12-003 | docs↔code 漂移（A/K） | 校验 docs/arch/INDEX.md §1 表中 est_tok 估算值与实际文件 Byte/Token 的比例漂移 | rejected | 由 make docs-gen-check 兜底 |
| GR-12-004 | docs↔code 漂移（A/K） | grep -n "Code.*Code = " pkg/apperr/apperr.go 与 docs/specs/00-Constitution.md §R2.5 错误码列表逐一求差集 | landed | 规则已就绪，见 lint-selftest.txt，commit 3cef8d5 |
| GR-12-005 | Schema/配置漂移（F） | grep -n "001-006_\*\.sql" docs/arch/00-Global-Dictionary.md 提取并校验 DDL 范围 | landed | 规则已就绪，见 lint-selftest.txt，commit 3cef8d5 |
| GR-12-006 | docs↔code 漂移（A/K） | 提取 docs/arch/M02-Storage-Fabric.md §2 tasks 表列定义与 internal/protocol/schema/007_tasks.sql 列定义求差集 | landed | 规则已就绪，见 lint-selftest.txt，commit 3cef8d5 |
| GR-12-007 | Schema/配置漂移（F） | grep -n "'official'" docs/arch/M13-bis-Extension-Registry.md 校验 origin 枚举列表内部一致性 | landed | 规则已就绪，见 lint-selftest.txt，commit 3cef8d5 |
| GR-2-003 | 接线断裂（G-bis） | grep -rn "security\.AuditRepo" internal/ 且测试不计入 | landed | L-13 tools/wiring_reachability_check.go，包级判据；2026-08-13 重写为 go list 闭包比对——初版跑 deadcode 需联网、与 make deadcode 完全重复，且正则匹配不到方法，而本类条目全是方法，结构上抓不到 |
| GR-3-003 | 其他 | AST 检查 Bootstrapper.Ignite 循环中 mod.Init 失败处理缺失逆序 Cleanup 逻辑 | rejected | 由单测覆盖，见 WP-5 |
| GR-5-001 | 接线断裂（G-bis） | 导出函数生产调用方可达性扫描，测试调用不计 | landed | L-13 tools/wiring_reachability_check.go，包级判据；2026-08-13 重写为 go list 闭包比对——初版跑 deadcode 需联网、与 make deadcode 完全重复，且正则匹配不到方法，而本类条目全是方法，结构上抓不到 |
| GR-5-002 | 接线断裂（G-bis） | 导出函数生产调用方可达性扫描，测试调用不计 | landed | L-13 tools/wiring_reachability_check.go，包级判据；2026-08-13 重写为 go list 闭包比对——初版跑 deadcode 需联网、与 make deadcode 完全重复，且正则匹配不到方法，而本类条目全是方法，结构上抓不到 |
| GR-5-003 | 接线断裂（G-bis） | 导出函数生产调用方可达性扫描，测试调用不计 | landed | L-13 tools/wiring_reachability_check.go，包级判据；2026-08-13 重写为 go list 闭包比对——初版跑 deadcode 需联网、与 make deadcode 完全重复，且正则匹配不到方法，而本类条目全是方法，结构上抓不到 |
| GR-5-005 | 接线断裂（G-bis） | 导出函数生产调用方可达性扫描，测试调用不计 | landed | L-13 tools/wiring_reachability_check.go，包级判据；2026-08-13 重写为 go list 闭包比对——初版跑 deadcode 需联网、与 make deadcode 完全重复，且正则匹配不到方法，而本类条目全是方法，结构上抓不到 |
| GR-6-003 | 其他 | 仅含 package 声明且无任何导出的源文件应清除 | rejected | 一次性处置，见 WP-10 |
| GR-7-003 | HE 不变量违反（B） | 在 context.WithTimeout 派生后执行沙箱/子进程操作前必须检查 parent ctx.Err | landed | 规则已就绪，见 lint-selftest.txt，commit 3cef8d5 |
| GR-8-003 | 其他 | `grep -n "rate limit exceeded" internal/extension/skill/skill_executor.go` | landed | 规则已就绪，见 lint-selftest.txt，commit 3cef8d5 |
| GR-8-004 | Taint 传播断点（D） | `grep -n "makeMCPToolAsyncFn" internal/extension/mcp/mcp_manager_tools.go` | rejected | 该条目本身是误报，见 §2 |
| GR-8-005 | 其他 | `grep -n "scanner.Buffer" internal/extension/mcp/mcp_client_stdio.go` | landed | 规则已就绪，见 lint-selftest.txt，commit 3cef8d5 |
| GR-9-003 | HE 不变量违反（B） | 提取 mux.HandleFunc 模式中的路径参数与 Handler 中 r.PathValue 调用的 key 集合，校验子集包含关系 | landed | 规则已就绪，见 lint-selftest.txt，commit 3cef8d5 |
