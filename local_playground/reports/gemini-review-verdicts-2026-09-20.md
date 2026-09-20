# Gemini 审核报告核验结论（2026-09-20）

> 范围：gemini-review-findings.md（131 条 GR）、gemini-review-design.md（10 条 GD）、lint-backlog.md（111 条可机械化待办）。
> 方法：逐条回到代码取证（grep/AST/运行测试），不采信报告自述；成立的按"系统最优解"修复，报告给出的修复方向有害时驳回并另行处置。

## 结论总览

| 类别 | 数量 | 说明 |
|---|---|---|
| GR fixed | 130 | 含"部分成立"已处置者；多数附回归测试 |
| GR open（待架构决策） | 0 | 全部已决策并实施（2026-09-20） |
| GR rejected | 1 | GR-7.1-002：未接线属实，但无消费者，报告所述后果不成立 |
| GD 已实施 | 4 | GD-13-002 / GD-14-001 / GD-14-002 / GD-14-003 |
| GD 保留（领先设计） | 4 | GD-13-001 / 13-005 / 14-004 / 14-005 |
| GD 不采纳 | 2 | GD-13-003 / GD-13-004（理由见明细） |
| lint-backlog | landed 3 / rejected 108 | landed 为 apperr_semantics_check 新增 L-11b/c/d；rejected 理由逐行写在状态列 |

### 报告未发现、本轮核验中挖出的高危缺陷（节选）

- **GraphRAG 文档入图谱管线从不落库**：GraphBuildPipeline.Run 抽出的实体/关系从未写入，整条文档→知识图谱链路不产生数据（已修，persistGraph）。
- **Reaper 租约过期判定失效**：RFC3339 与 datetime('now') 字符串比较，同日过期租约永不回收、仅跨 UTC 午夜批量回收；叠加 RenewLease 零调用，长任务会在午夜被随机误杀（已修 + HoldLease 心跳）。
- **apperr 哨兵按 Code 匹配**：ErrRequiresApproval（任意 403 被当作"需审批"走 202）、ErrAllProvidersFailed / ErrTaintBlockedEgress（任意 CodeInternal 命中）、ErrReplanExhausted（Provider 限流被当作重规划耗尽）、ErrStaleBlackboardLease/ErrTaskNotOwned。新增 apperr.NewSentinel（按身份比较）并以 L-11d 门控防复发。
- **VFS 情景载荷目录会被 GC 整目录删除**：logs/events 被 rebuildManifests 当作任务工作区，一旦 GC 接线即 7 天后删除（已修，仅标记目录入清单）。
- **合成技能可覆盖内置工具 / 已安装受信技能**：GapFillWorker 把无实现的合成工具按名覆盖进活跃工具表（已修，改为待审候选）。
- **Marketplace pkg.ID=".." 递归删除安装根的父目录**（已修 + 测试）。
- **Windows OTA 更新脚本从未启动**，且 200ms 抢跑 os.Exit 跳过优雅关停（已修）。
- **SysAdmin 偏好热更新对 nil 接口调方法**（生产必 panic，已修并注入 agent-0）。
- **合成评测用例写入非法分区 "synthetic"**，PutCase 恒失败（已修）。
- **上下文压缩把 system 前缀（含安全规约）压成 assistant 摘要**（GD-14-001，已修）。

## 架构决策（已全部完成 2026-09-20）

1. **GR-6.1-002 NativeOS 沙箱层级**：启用 NativeOS 为 Tier-0 降级路径。ADR-0008 追记决策四（bwrap/Seatbelt ≠ 无隔离裸 exec），assign.go 改为 hwTier==0 + Container → SandboxNativeOS（CapPrivileged 保留 fail-closed）。M07 §4.2、03-Agent-Pattern §AGENT-7 同步追记。
2. **GR-6.2-004 Saga 补偿声明**：废弃 Micro-DAG 中 "write_* 必须声明 CompensationAction"（M04 §4.3 追记）。write_local 由 VFS 快照回滚，write_network/privileged 由 HITL 覆盖（inv_M4_06）。validator.go 保持 slog.Info 观测。ADR-0088 追记。
3. **GR-8-001 SkillSelector**：删除 boot_tools.go 中 skillSelector 死代码。接口/实现标注 Deprecated。M04 §5、M06 §3.1 追记被 M13-bis CompositeCatalog + search_tools 替代。
4. **GR-7.2-003 / GR-1.1-003 GraphWriter 与社区摘要**：删除 GraphWriter（写不存在的 entities 表），保留 ProviderLLMClient。mutation_bus_execute.go 白名单清理。Leiden 社区摘要代码保留但标记未接线（待 FeatureGraphRAGFull 门控）。M10 §2.7 追记。
5. **GR-10.1-001 成本事件**：Router 产出 llm.call.recorded 事件（pb.LLMCallPayload Protobuf）。CostReporter 改读 Protobuf + topic 精确匹配。删除零调用方 AggregateTokenCosts。M01、M13 追记。

## 逐条明细

| ID | 判定 | 依据/处置 |
|---|---|---|
| GR-1.1-001 | 成立 P0 | UpsertCheckpoint/Get*/ListCheckpointsByTask 均漏 reason；ListByStatus 读 reason 恒空，reconciler_handoff.go:101 全部跳过。已修：四处读写补 reason(+List 补 resume_ctx_json)，新增 repo_task_checkpoint_test.go 用 SSoT DDL 往返 |
| GR-1.1-002 | 成立 P1 | sys_config.value TEXT 字典序比较，9→10 冻结。已修 CAST INTEGER，新增 TestSaveCursor_CrossesDigitBoundary |
| GR-1.1-003 | 成立（潜伏）P1→P3 | 通用 insert/upsert/delete 模板唯一调用方 GraphWriter（写不存在的 entities 表），而 GraphWriter 与 Clusterer.Cluster 均无生产调用方。与 GR-7.2-003 合并处置。已决策：GraphWriter 已删除，validateTable 白名单移除 entities/episodic_memory |
| GR-1.1-004 | 成立 P1→P3 | 无 Stop；Background 使停机时在途 Embed 可能永挂。已修：EmbeddingBatcher.Stop()/failPending/ErrBatcherStopped，boot 以 WithoutCancel 启动并入 SubstrateBundle.EmbedBatcher，main §14 与 performHotRestart 显式 Stop |
| GR-1.1-005 | 成立 P2 | Recover 只在 Apply 失败时执行且吞原始错误。已修：先 Recover 后 Apply，Apply 错误上抛 |
| GR-1.1-006 | 成立 P2 | Get/Scan/ListPreferences 走写连接；Scan 迭代期间 Put 会自锁。已修：改走 readDB；删除零调用 SQLQuerier()（返回写连接且接口含 Exec） |
| GR-1.1-007 | 部分成立 P2 | GraphTraverse 经 protocol.GraphTraverser 接口暴露（非孤立）；GraphDeleteEdges 确无调用。另发现：deleteCognitiveIndex 删 "ep_"+uuid，而写入侧 FTS/Vec 主键为裸 uuid，归档后索引删除恒落空。已修：ID 对齐 + ForgettingManager.WithGraphEdgeDeleter 接 SurrealStore，归档时删 "episodic:"+uuid 出边 |
| GR-1.1-008 | 成立 P3 | Priority 1 进 Low 队列 + 无超时。已修：PriorityHigh/Low 常量 + 30s 同步超时 |
| GR-1.2-001 | 成立 P1 | 按 featureRule 语义（≥Degrade 全量/[Min,Degrade) 降级）判定顺序倒置。已修 + feature_gate_test.go（三态 + 规则表 Degrade>Min 不变量） |
| GR-1.2-002 | 成立 P1 | SetLastValue 漏刷 staleness。已修 |
| GR-1.2-003 | 成立 P2 | ActiveAgentsCount 无写入方。已修：Agent.Run 入口 Add(1)/defer Add(-1) |
| GR-1.2-004 | 成立 P2 | 实际漏暴露 8 个（报告 4 个 + 4 个 updater 签名计数）。根因是 OTel/legacy 两份手写清单。已修：metrics_counters.go 表驱动单一清单；metrics_counters_test.go 以 AST 校验所有 Global*Total 必须入表 |
| GR-1.2-005 | 成立 P3 | 锁内回调监听器。已修：recordLocked 锁内产出告警+监听器快照，锁外回调 |
| GR-2.1-001 | 成立 P1 | 结构层拒绝 >Low 后内容层 >=Medium 不可达；M11 规范文档本身两句互斥。按 M11 设计原则收敛为扫描 TaintLow（None 豁免）。先改 M11 §2.5（追记复核），再改代码；测试改写 |
| GR-2.1-002 | 部分成立，修复方向驳回 | 实例级 taintLevel 生产恒 0，分支死路属实；但"读 ctx 污点"有害：gateway 给每个请求 ctx 注入 Medium/High，LLM Provider 出站会被默认空白名单全部拒绝。污点外发由 Gate4/TaintEgressCheck 按载荷执行（M11 §6）。保留实例级机制，DialContext 写明理由 |
| GR-2.1-003 | 成立 P1 | 未排序 + 未处理重叠。已修：nonOverlappingByStart；顺带修 phone_cn 正则吞空格（原测试固化了该缺陷，已订正） |
| GR-2.1-004 | 成立 P2 | KILLSWITCH 文件无轮询方（M11 §4.1 要求 <500ms）。已修：boot 500ms 轮询；已 FullStop 时不重复迁移 |
| GR-2.1-005 | 成立 P2 | records 常驻内存无上界。已修：Record 按 epochBytes 自驱动 rotateLocked |
| GR-2.2-001 | 成立 P1 | 底层流未绑定可取消 ctx。已修 |
| GR-2.2-002 | 成立（更严重） | 漏检 winBreaker 属实。同源更重缺陷：6 处选路循环对每个候选调用 cb.Allow()，冷却期满候选在"仅被比较未被选中"时占住唯一 HalfOpen 探测权，永不 Record → 永久排除；ModelID()/Tokenizer()/PickProviderName() 只读调用同样触发。已修：circuitBreaker.Available() + selectBest 统一选路 + peekBest；circuit_probe_leak_test.go |
| GR-2.2-003 | 成立 P1→P3 | 已改为 CredentialPool 取用模型（Pick/CredFn/ClearBytes/RecordResult） |
| GR-2.2-004 | 部分成立 | 空响应零兜底属实已修。"缺超时永久阻塞"不成立：llama FFI 同步不感知 ctx，WithTimeout 为安慰剂；Rust 侧 max_tokens 默认 512 封顶。改为入 FFI 前检查 ctx.Err() |
| GR-2.2-005 | 成立 P2 | 删除 TrackStreamCost 硬编码 256KB 二次截断 |
| GR-2.2-006 | 成立 P2 | 删除 Router 孤儿字段 rateTracker/client，M01 §10 订正为"库组件、未接生产" |
| GR-3-001 | 成立 P1（更严重） | 停机顺序倒置之外：db_writer 绑信号 ctx、finalFlush 用已取消 ctx 整批丢弃；Close 后 Submit 会 send on closed channel panic。已修：writer 脱离信号 ctx、closeMu+ErrDatabaseWriterClosed、finalFlush、main §14 顺序重排；mutation_bus_close_test.go |
| GR-3-002 | 成立 | 与 GR-8-001 同根因，合并至批次 8 |
| GR-3-003 | 成立 P3 | 删 GraphRAGDailyBudget，make gen-threshold-examples 重生成 |
| GR-3-004 | 成立 P2 | 补 --stage/--smart 解析 |
| GR-4.1-001 | 成立 P1 | 等待 effectRunning 期间同时消费 effectDone，effectIdle 信号替代 1ms 轮询 |
| GR-4.1-002 | 成立 P1 | Agent.sessionTaint() 取会话累计污点；learning 取 max(Medium, MaxTaintLevel)。另修：memory facade 统一包 CodeInternal 使存储熔断永不触发，改 Wrap(CodeOf(err)) |
| GR-4.1-003 | 成立 P1 | TaskModel/GroundingGap 移出 TaintNone 指令区，走 WriteUserData 围栏 |
| GR-4.1-004 | 成立 P1 | 召回记忆改 user 角色 + Spotlighting |
| GR-4.1-005 | 成立 P1 | S_VALIDATE runBlindZoneHITL：含副作用计划发 HITL；网关未装配降级放行告警 |
| GR-4.1-006 | 成立 P2 | capability_gap 改 emitOutbox；m9_storage_degraded 无消费者且存储不可用时写不进，改计数器 GlobalMemoryPersistenceFailuresTotal |
| GR-4.1-007 | 成立 P3 | 删 WorldModelUpdater，订正 agent/CLAUDE.md |
| GR-4.2-001 | 成立 P0（更严重） | wasmtime 在场时 CodeAct 被送进 WasmtimeExecute 必然失败，默认配置下整体不可用。Tool 标 SideProcessSpawn 强制 L3；原测试固化缺陷已改写 |
| GR-4.2-002 | 成立 P1 | ImportFrom + 两遍扫描 + 危险函数表统一 |
| GR-4.2-003 | 成立，修复方向修正 | 透传 Temperature/ResponseFormat；Model 仅非空覆盖，boot 删除占位名 "default-vlm" |
| GR-4.2-004 | 成立 P1 | xwd/convert/xdotool CommandContext(10s) + sanitizeX11Env |
| GR-4.2-005 | 成立（更严重） | CodeAct 依赖的 protocol.ToolExecutor 无生产实现，boot 传 nil，审计从未写入。收窄为 AuditRecorder 注入 AuditTrail；载荷补 code/output |
| GR-4.2-006 | 成立 P3 | 删 internal/action/provider.go |
| GR-4.2-007 | 成立（文档已记载的未交付功能） | 不做假接线，M07 追记 |
| GR-4.2-008 | 成立 P3 | rm 只解析选项位 |
| GR-4.2-009 | 成立 P3 | 删两零引用哨兵 |
| GR-5.1-001 | 部分成立 P0→P3 | 列名确错；但生产 SQLiteStore 无 BeginTx 走非事务分支。修列名 + forgetting_schema_test.go |
| GR-5.1-002 | 成立 P1 | MarkCold 秒/毫秒失配（change_log changed_at 同步改毫秒） |
| GR-5.1-003 | 成立 P1 | 根因是 ScoredEvent.Event 为 any。新增 ScoredEvent.EventPtr()（值/指针皆可），全仓 33 处断言统一替换 |
| GR-5.1-004 | 成立 P1 | 向量路径 Source 改 "episodic:{uuid}"（空时 episodic_row:{id}） |
| GR-5.1-005 | 成立 P2 | Flush 改单事务，失败整批回滚重试；无事务降级只回填失败项 |
| GR-5.1-006 | 成立 P2 | sync.Once 保护惰性构造 |
| GR-5.1-007 | 成立 P2 | 改 || 并补 fp0-fp2，兑现两两相似 |
| GR-5.1-008 | 成立，修复方向修正 | 报告建议改 "episodic:" 前缀同样错误：tombstone 来自 cleanupWithKV 扫描的 events:session:{sid}:{ts}_{seq} 键，无法由 id 反推。改为 tombstone 记录真实键并按键删除 |
| GR-5.2-001 | 成立 P0/P1（且更多） | 三点属实；另发现抽帧失败时返回伪造的 mock base64 帧冒充成功（原测试固化了该行为）。已修：本地路径过白名单+黑名单；远程经 SafeDialer 下载（200MB 上限）后本地处理，ffmpeg 永不联网；失败返回错误；tool.yaml 补 network-call；测试重写 |
| GR-5.2-002 | 成立 P0 | 网络权限改由 spec.Capability 决定。另发现 schema.json（module_path/args/mount_dirs）与实现（code/input/workspace）完全不一致，工具经 schema 校验后不可用；schema 按实现重写并 additionalProperties=false |
| GR-5.2-003 | 成立 P1 | 新增 guard.CheckWritablePath（白名单+黑名单+软链解析后复核），write_file/str_replace_editor/multi_edit/notebook_edit 统一使用 |
| GR-5.2-004 | 成立 P1 | Skill 输出污点 = max(入参, TrustTier.TaintLevel()) |
| GR-5.2-005 | 成立 P1 | rebuild 以 TrustUntrusted 取全量，出口再过滤 |
| GR-5.2-006 | 部分成立 | 指标缺失属实已补（与 InProcess 同口径）；污点"丢失"影响有限——ExecEnvelope 已做 only-up，结果仍补回输入污点 |
| GR-5.2-007 | 成立 P1 | COW 重写未命中时 fail-closed 走模拟结果 |
| GR-5.2-008 | 成立 P2 | refillAt 只读一次作为 CAS 期望值 |
| GR-5.2-009 | 成立 P3 | RLock 取快照 |
| GR-5.2-010 | 成立 P3 | 改 CodeNotFound（apperr.Is 按 Code 比较，原 CodeInternal 会让任意内部错误 errors.Is 命中） |
- GR-6.1-003 真实 → gcWorker(ctx) select ctx.Done；time.Sleep 改 select。
- GR-6.1-004 真实 → StartPeriodicGC(1h) + boot 注入 tasks 非终态 task_id/session_id；查询失败 fail-closed 跳过。
- GR-6.1-009 真实且更严重（报告未发现）：logs/events 情景溢出载荷目录重启后被建为 task 清单，一旦 GC 接线会被 7 天整目录删除。修复：仅有 .wm_created_at 标记的目录建清单；墓碑重新入队；_ephemeral_scripts 跳过。测试 workspace_manager_gc_test.go。
- GR-6.1-005 真实 → spawn 后重新加锁二次校验，复用已有存活会话并 kill 多余进程；容量复检。
- GR-6.1-006 部分成立：ExecEnvelope 路径已 only-up；SandboxRouter.Execute 路径确实透传 → 在路由出口统一 only-up。
- GR-6.1-007 真实（Windows）→ filepath.IsLocal；加 inner traversal 测试。
- GR-6.1-008 真实 → MkdirAll(filepath.Dir(path))；新增子目录测试。
- GR-6.1-010 真实 → WriteToolHints 改写 ZoneMutableSkill（学习产物不应进 Immutable 区），接口注释与测试同步。
- GR-6.2-001 成立（前提需修正：StartExecution 生产零调用，任务一直 claimed；真正问题是 RenewLease 全仓零生产调用）。修复：RenewLease 覆盖 claimed/running 且不再 version+1（原 +1 会让 SideEffectPreCheck 的 claimedVersion fencing 在首次心跳后失效）；新增 orchestrator.HoldLease（心跳+stale 即取消执行 ctx），接入 Default/MCPA2A/Debate/WorkflowStep 四个 Worker。
- 隐藏缺陷（报告未发现，P1）：Reaper `expires_at < datetime('now')` 用 RFC3339('T') 与 datetime(' ') 字符串比较，同一 UTC 日内的过期租约永远判不出，仅跨午夜批量回收（长任务在午夜被随机误杀、真卡死任务最长滞留 24h）。修复 datetime(expires_at)；lease_keeper_test 覆盖。
- 隐藏缺陷：ErrTaskNotOwned/ErrStaleBlackboardLease 均为 CodeInternal，apperr.Is 按 Code 比较 → 任意 DB 错误都匹配。改 CodeForbidden/CodeConflict。
- GR-6.2-002 成立 → 新任务 Claim+StartExecution；Execute 返回 suspend 时 SuspendForHITL 挂起（不占租约、不被租约 Reaper 回收）；子任务终态唤醒走 ResumeFromHITL（SQL 校验 claimed_by）；inFlight/pendingWake 防丢失唤醒。
- GR-6.2-003 成立（当时死代码路径，本轮 Debate 已启用 StartExecution）→ 回读 claimed_by 比对。
- GR-6.2-004 已决策：废弃 Micro-DAG 中 "write_* 必须声明 CompensationAction"（M04 §4.3 追记复核）。validator.go 保持 slog.Info 观测不升级为拦截。write_local 回滚由 VFS 快照承担，write_network/privileged 由 S_VALIDATE BlindZoneHITL 覆盖（inv_M4_06）。ADR-0088 追记：Micro-DAG ExecNode.Compensation 保留但不强制校验。
- GR-6.2-005 成立 → HoldLease 通过窄接口 cancelRegistrar 注册/注销（新增 UnregisterCancelFunc），未扩大 protocol.Blackboard 接口。
- GR-6.2-006 成立，且范围更广（Default/Debate Worker 回放期执行 headless Agent 会抢占被恢复 Agent 的回放 LLM 记录）→ waitReplayDone：回放期推迟认领（不跳过，task_posted 只投递一次）。
- GR-7.1-001 成立 → mu + 快照；Embed 在锁外；-race 并发测试。
- GR-7.1-002 部分成立：Run/PruneStaleCache 确未接线，但 memPressure 唯一消费者 MemoryAgent 生产零构造，idempotent_cache 生产零写入（CheckIdempotent/RecordExecution 零调用）——"背压失效/缓存无限膨胀"不成立。仅启动 Run 只会更新无人读取的原子量，不做无效接线；记为 swarm 死代码待清理项。
- GR-7.1-003 成立 → recent 为空时以 DB 近期版本为样本，候选全空才返回；新增测试。
- GR-7.1-004 成立且更严重：GapFillWorker 把只有 schema、无实现的合成工具挂进 InMemoryToolRegistry，Register 按名覆盖——toolName 来自 LLM 报错文本（可被注入操纵），可覆盖内置工具；SkillRegistry.Register 为 upsert，可覆盖已安装受信技能。修复：不再挂进活跃工具表；工具名用请求名而非 LLM raw.Name；TrustUntrusted、不硬编码 InProcess；SkillMeta 以 Deprecated 待审候选落库；同名存在不覆盖；失败计数接回 metrics。
- GR-7.1-005 成立 → ctx 透传。
- GR-7.1-006 成立 → 记录真实 err。
- GR-7.2-001 成立 → chunk_type IN (doc/chap/para_summary)。
- GR-7.2-002 成立 → 空 SectionPath 放行；同时修正仲裁输出按 map 迭代打乱检索排名（改为保序剔除败者）；测试。
- GR-7.2-003 已决策（与 GR-1.1-003 合并）：删除 GraphWriter（写不存在的 entities 表，与 DDL SSoT semantic_entities 不兼容），保留 ProviderLLMClient。mutation_bus_execute.go 白名单移除 entities/episodic_memory。Clusterer.Cluster 方法签名改为函数注入（解除 GraphWriter 类型依赖）。Leiden 社区摘要代码保留但标记未接线，待 FeatureGraphRAGFull 门控。M10 §2.7 追记。
- 隐藏缺陷（P1，报告未发现）：GraphBuildPipeline.Run 从不持久化 Phase1/2 抽出的实体与关系——文档→知识图谱管线实际不产生任何图数据（Phase5 概念建边反查源实体 DBID 也因此恒失败）。修复：persistGraph 经 SemanticMemory 落库（已存在实体不重写 properties，外部文档 ≥TaintMedium，悬空端点不建边，共现回退边不落库）；collectEmbeddings 下标错位修复；测试 persist_graph_test.go。
- GR-7.2-004 成立（且 consolidation 共享抽取同样受影响：传的是会话 ID）→ Extract 增加 docText 参数并返回 inferred 标记。
- GR-7.2-005 成立 → Watch 不可用时退化为周期重同步（两者都无才报错）。
- GR-7.2-006 成立 → SELECT 带出 taint_hmac/source_uri，SearchGraph 以 verifyChunkTaint 校验（graphrag 不持密钥，与其他检索路径同一校验点）。
- GR-7.2-007 成立（且为内存泄漏：进程级 map 无读者无限增长）→ 删除内存 linker，跨文档链接由 semantic_entities (type,name) 唯一键在落库时实现。
- GR-7.2-008 成立 → 随机边界 + ExtractJSONBraces。
- GR-6.1-001 成立 → SandboxRouter.trackExec 由 ExecEnvelope 生产路径共用（见上文）。
- GR-6.1-002 已决策：启用 NativeOS 为 Tier-0 降级路径。ADR-0008 追记决策四（bwrap/Seatbelt 有隔离边界 ≠ 无隔离裸 exec），assign.go 改为 hwTier==0 + Container → SandboxNativeOS（CapPrivileged 保留 ErrTier0SandboxLimit）。M07 §4.2、03-Agent-Pattern §AGENT-7 同步追记。go test -run TestAssignSandboxTier 通过。
- GR-8-001 已决策（与 GR-3-002 合并）：删除 boot_tools.go 中 skillSelector 死代码。HybridRetriever/SkillSelector 接口标注 Deprecated，保留源码供未来参考。M04 §5、M06 §3.1 追记被 M13-bis CompositeCatalog + search_tools 元工具替代。
- GR-8-002 成立 → PluginInstaller.WithPolicyGate，逐个子 MCP 独立 Review（ext_type=mcp），拒绝/无网关 skip+Warn 不中断父（extension/CLAUDE.md 硬约束 3）；boot 注入 sb.Gate。
- GR-8-003 成立（且比报告更严重：ID==".." 时 RemoveAll 删除基目录的父目录）→ filepath.IsLocal 单段目录名 + Command 本地相对路径校验；测试。
- GR-8-004 成立 → Instructions 填 SKILL.md 全文（与落盘同一 renderSkillMD），Runtime=script；SkillMeta 无 Description 字段（报告字段名不准确）。
- GR-8-005 成立 → MCPManager.ServerToolNames 取真实 mcp__<server>__<tool> 名；未就绪时给出命名格式。
- GR-8-006 成立（前提需修正：ExtensionID 多为随机后缀，碰撞面主要在调用方显式传固定 ID 时）→ 以 inst.ID@CreatedAt 标识安装代次。
- GR-8-007 成立 → FSM 失败向上返回（保留原错误码）。
- GR-9.1-001 成立 → 键改 clientIP:clientType；新增单 task 30s 防抖（M13 §1.2.5）。
- GR-9.1-002 成立（且 OTAUpdater 方法集与 updater.Manager 不符、全仓无实现）→ 删除只写字段与死接口，SetUpdater 仅转交 SysAdminHandler。
- GR-9.1-003 成立 → 删除孤儿注释。
- GR-9.2-001 成立（报告称 /v1/models 已注册不实——两条都未注册，且 HandleOpenAIModels 不存在）→ 注册 POST /v1/chat/completions。
- GR-9.2-002 成立且更严重：HandleSetPreference 对 nil 接口直接调用方法，生产必 panic → Server.SetAgentController 注入 agent-0；preferences 判空。
- GR-9.2-003 成立 → 普通哨兵 errHandshakeHandled，调用方识别后直接返回。
- GR-9.2-004 成立 → ExtensionID/RuntimeID=server ID，删除时同步删实例行；Authorize 后 BypassAuth 避免重复评估。
- GR-9.2-005 成立 → SetToolRegistry/SetMCPManager 同步回填 PromptService。
- GR-9.2-006 结论相反：不是死代码而是漏注册——M13 与 Web UI 均使用 /v1/{plugins,apps,mcp}/create，生产 404；skills/create 同路径两种语义（CLI intent / UI 手工）。修复：注册三条路由，skills/create 按请求体分派。
- GR-9.2-007 成立 → apperr.HTTPStatus(CodeOf(err))。
- GR-10.1-001 已决策：Router 产出 llm.call.recorded 事件（pb.LLMCallPayload Protobuf 序列化），经 EventLogger.AppendEvent 异步写入 events 表。CostReporter 改用 proto.Unmarshal 解析 payload + topic 精确匹配 'llm.call.recorded'。单位已改 UnixMilli + rows.Err() 补 F-7 门控。删除零调用方 AggregateTokenCosts 及接口签名。M01、M13 追记。
- GR-10.1-002 成立 → training/validation 分区按整分区扫描（key 中 role 段是写入来源，读者角色只做访问控制）；meta_holdout 读写同角色，保留收窄。隐藏缺陷：boot 合成用例写 "synthetic" 非法分区，PutCase 恒失败 → 改 training（M09 §合成用例路由 Training Set）。
- GR-10.1-003 成立 → 引擎按 IncidentPayload 契约组装 input/expected/details。
- GR-10.1-004 成立 → 共享 cond 下 Signal 改 Broadcast。
- GR-10.2-001 成立 → 15 个适配器 Send 配置缺失返回 CodeInvalidInput；telegram 非 200 返回错误；未知渠道类型报错；清除 "log event" 伪错误参数。
- GR-10.2-002 成立 → closeOnCancel(ctx, conn) 接入 8 个 WebSocket 长连接。
- GR-10.2-003 成立且更严重：更新脚本写出后从未被启动（Windows 更新根本不会生效），200ms os.Exit 抢在优雅关停前 → osutils.StartDetachedWindowsScript 启动脚本，删除抢跑退出；performHotRestart 在 Windows 上优雅关停后 exit(0)（syscall.Exec 不支持）。测试注入 startScriptFn。
- GR-10.2-004 成立 → 删除零实现零消费的 ChatRepo/AgentInfer。
- GR-10.2-005 成立 → 无长连接/队列满/ctx 取消均返回错误；poller 退出 CompareAndDelete 注销队列。
- GR-10.2-006 成立 → 超时返回 context.DeadlineExceeded；hooks.go 原按外层 Background ctx 判超时（永不命中）一并修正为 errors.Is(runErr)，返回 CodeTimeout。
- GR-11-001 成立（catch_unwind 兜住但持锁 panic 毒化 INIT，之后永远无法重试）→ 返回错误码；cargo check 通过。
- GR-11-002 成立 → 02-Rust-FFI.md RUST-3 目录树/RUST-4 依赖白名单订正（追记）。
- GR-11-003 成立 → ffi-abi.md §2.1 两种字符串约定并存、§6 符号位置订正（追记）。
- GR-11-004 成立 → 删除 // +build ignore。
- GR-12-001 成立 → g_inv_07 按 BuildIdempotencyKey 订正（规范落后于已统一的实现；行数不变不破坏 §跳读）。
- GR-12-002 部分成立：水位/容量阈值已存在（报告称缺失不实）；parquet 属实 → sqlite_attach。
- GR-12-003 成立 → M05 §5-bis 8 工具 + 7 参签名。
- GR-12-004 成立 → M12 核心文件路径订正。
- GR-12-005 成立 → M06 存储路径订正为 SQLite 直写。
- GR-12-006 成立 → M11 RedactBlock/ErrPIIDetected 标注未实现。
- GR-12-007 成立 → state.yaml 节名 m13_interface，6 处文档/注释引用同步。
- GR-12-008 成立 → M07 补入口 5（A2A）、入口 6（Extension），45 个 / 6 入口。

## 设计挑战（GD）
- GD-13-001/13-005/14-004/14-005 领先设计(保留)：2026-09-20 代码取证复核确认为最优解。
  - GD-13-001（崩溃恢复双轨架构）：ReplayMode 在 agent_execute_effect/outbox_worker/agent_context_compaction/agent_execute_dag 四处物理短路，有 4 个专用回归测试覆盖。最优。
  - GD-13-005（Session/Transport 解耦）：session 包零 `net/http` 导入，有 AST 门控 `Test_inv_M13_SessionPkgNoHTTP` 防退化。Sink 接口已有 sseSink/BufferSink/test spy 三种实现。最优。
  - GD-14-004（五级污点 + Cedar 策略）：L-15 门控禁裸 TaintedString 构造、L-17 PolicyGate 门控、inv_M7_01 Capability Token 门控三重 AST 静态保护。最优。
  - GD-14-005（可编程核心记忆块）：SQLCoreMemoryStore 六方法完整（Get/Set/Delete/List/Replace/Describe），集成 WorkingMem + ZoneCoreMemory 上下文装配 + CoreMemoryBlockMaxKB 配额 + taintLevel 追踪。最优。
- GD-13-002 成立 → llm.ProviderRegistry.ReplaceAll（锁外构建、单次交换）；LoadProvidersFromDB 先收集再原子替换，查询中途出错不留半套；并发无空窗测试。
- GD-14-001 成立（且涉及安全：ZoneImmutable 规约被压成 assistant 摘要）→ compact.SplitPinnedHead 钉住开头连续 system 消息，Agent 热路径与网关压缩两处接入；测试。
- GD-14-002 成立 → makeSamplingHandler(server, trust)：PolicyGate mcp_sampling（仅 TrustOfficial+，内置规则 + soft_constraints.cedar 同步）、每服务端 20K token/分钟预算、单次 4096 封顶、system 角色降级为 user；测试。
- GD-14-003 成立 → schemavalidate.ValidateAgainst 接入 SchemaValidateInterceptor（required/type/properties/items/enum；schema 不可解析时不拦截）。
- GD-13-003 暂不采纳：前提（恢复在 HTTP 服务前串行）成立；且本轮 waitReplayDone 正依赖进程级标志让后台 Worker 在回放窗口推迟认领。若将来引入服务后的后台增量恢复，再改为 ctx 级回放标志。
- GD-13-004 不采纳（补注释）：已提交事务在 WAL 中持久由新进程自动恢复；fd 为 O_CLOEXEC，exec 后锁释放；sql.DB.Close 幂等，重试会伪装成功；中止 exec 会让系统卡在未服务状态。
