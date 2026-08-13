# 代码轨道 批次 7 审核报告

| ID | 严重级/动作 | 模块或对象 | 一句话标题 | 置信度 | 可机械化 |
|---|---|---|---|---|---|
| GR-7-001 | P0 | internal/knowledge | KnowledgeBase.Search 缺失 TaintMax 校验导致高污染数据穿越安全隔离区 | 高 | 是 |
| GR-7-002 | P1 | internal/learning | Engine.Start 对 taskEvents/versionEvents 通道关闭误用 return nil 导致后台自进化引擎提前异常关停 | 高 | 是 |
| GR-7-003 | P2 | internal/swarm | PlannerPool.workerEngineA 派生超时 context 后未校验 parent context 导致取消信号下仍然运行高消耗沙箱构建 | 高 | 是 |

置信度分布声明: 本批次所有 3 条发现均基于 Go 源码物理行精确定位（包含安全门控缺失、通道关闭异常路径及 Context 派生超时逻辑），且全部通过 §2-A 强制反证与全流程调用链搜检，不依赖运行时未知条件，故置信度均为高。

### [GR-7-001] KnowledgeBase.Search 缺失 TaintMax 校验导致高污染数据穿越安全隔离区
- 严重级: P0
- 模块: internal/knowledge（层: L2）
- 位置: internal/knowledge/rag_retrieval.go:340
- 违反规则: HE-2 | HE-7
- 置信度: 高
- 可机械化: 是（建议规则: KnowledgeBase.Search 结果组合前必须校验 chunk.TaintLevel <= req.TaintMax，超出必须过滤）
- 反证: 已查 cmd/polaris/boot_knowledge.go、internal/knowledge/rag_retrieval.go:311-370、DefaultHybridRetriever.Search(rag_retrieval.go:76)、HybridRetrieverImpl.Search(retriever.go:103) 及 ContextExpander.Expand(rag_retrieval.go:167) 四处。KnowledgeBaseSearchRequest 明确定义了 TaintMax 门控字段，但在 KnowledgeBase.Search 中，从 kb.retriever.Search 返回的 chunks 未经任何 TaintLevel <= req.TaintMax 的校验即直接 append 入 allChunks 并由 ContextExpander 扩展返回。DefaultHybridRetriever/HybridRetrieverImpl 的 RetrievalConfig 均不包含 TaintMax 过滤逻辑。全查 boot_knowledge.go 确认 KnowledgeBase.Search 为外部唯一检索入口，TaintMax 过滤在整个调用链中物理缺失，导致高污染数据可绕过安全门控渗入 Prompt。
- 问题: KnowledgeBaseSearchRequest 在接口契约中定义了 TaintMax 字段作为控制最大允许污点级别的安全门控，但在 KnowledgeBase.Search 的实现中，检索得到的 chunks 未经 TaintLevel <= req.TaintMax 的断言与过滤直接汇总并送入 ContextExpander 扩展。攻击者注入的高污点（TaintHigh/TaintCritical）外部知识块可绕过 TaintMax 门控直接渗入 Prompt。
- 证据: internal/knowledge/rag_retrieval.go:322-331
  ```go
				chunk := Chunk{
					ID:          c.Source,
					DocID:       c.Source,
					Content:     c.Content,
					TaintLevel:  int(c.TaintLevel),
					TaintSource: c.Metadata["taint_source"],
					SourceURI:   c.Source,
				}
				allChunks = append(allChunks, chunk)
  ```
- 修复方向提示: 在 KnowledgeBase.Search 遍历 chunks 时，追加 if req.TaintMax > 0 && int(c.TaintLevel) > req.TaintMax { continue } 条件过滤。

### [GR-7-002] Engine.Start 对 taskEvents/versionEvents 通道关闭误用 return nil 导致后台自进化引擎提前异常关停
- 严重级: P1
- 模块: internal/learning（层: L2）
- 位置: internal/learning/engine.go:234
- 违反规则: HE-5 | HE-6 | 维度L-生命周期
- 置信度: 高
- 可机械化: 是（建议规则: Engine.Start 中 select-case 读取通道时，!ok 应对通道置 nil 继续循环，禁止直接 return nil）
- 反证: 已查 cmd/polaris/boot_*.go、internal/bootstrap/、internal/learning/engine.go:228-336 四处。Engine.Start 是 M9 自进化引擎的主后台工作协程，在一个 select 循环中同时承载 taskEvents 消费、heuristicEvents 消费、midTicker 课程生成 (每2min)、l3Ticker 策略漂移检测 (每10min)、l4TriggerCh、versionEvents 及 evalEvents 7 条并发管道。核对 heuristicEvents (:249)、l4TriggerCh (:296)、evalEvents (:323) 的处理，在 !ok 时均采用 e.xxx = nil; continue 保持后台主循环继续运行。唯独 taskEvents (:234) 与 versionEvents (:302) 在 !ok 时直接 return nil，导致上游任一事件源关闭即中断整个 Engine.Start 协程，使课程生成与漂移检测静默挂起。
- 问题: Engine.Start 管理着包含内环、中环、外环及 L3/L4 检测的复合生命周期。但在处理 taskEvents 和 versionEvents 通道关闭 (!ok) 时，使用了 return nil 而不是 e.taskEvents = nil; continue。一旦生产环境中的某个上游事件生产者关闭了通道，会导致整个 Engine.Start 协程静默退出，中断所有定时课程生成与策略漂移检测。
- 证据: internal/learning/engine.go:234-237
  ```go
		case ev, ok := <-e.taskEvents:
			if !ok {
				return nil
			}
  ```
- 修复方向提示: 将 taskEvents 和 versionEvents 的 !ok 分支修改为 e.taskEvents = nil; continue 与 e.versionEvents = nil; continue。

### [GR-7-003] PlannerPool.workerEngineA 派生超时 context 后未校验 parent context 导致取消信号下仍然运行高消耗沙箱构建
- 严重级: P2
- 模块: internal/swarm（层: L2）
- 位置: internal/swarm/planner/pool.go:175
- 违反规则: HE-2
- 置信度: 高
- 可机械化: 是（建议规则: 在 context.WithTimeout 派生后执行沙箱/子进程操作前必须检查 parent ctx.Err()）
- 反证: 已查 cmd/polaris/boot_*.go、internal/bootstrap/、internal/swarm/planner/pool.go:175-193 四处。workerEngineA 在 L175 使用 buildCtx, cancel1 := context.WithTimeout(ctx, 30*time.Second) 派生超时 context，但在 L181 发起 p.sandbox.Execute(buildCtx, "go", []string{"build", tmpDir}, wd, 30*time.Second) 之前未检查父 context (ctx.Err())。沙箱内部会重新根据传入的 timeout 参数构造独立超时，导致在父 ctx 已被取消（如 HTTP 请求超时或用户取消）时，系统仍会无意义地启动外部 go build / go test 编译测试子进程，造成 CPU 资源浪费。
- 问题: PlannerPool.workerEngineA 在派生 buildCtx 和 testCtx 之后，直接发起沙箱编译与测试，未检查父 context (ctx.Err()) 是否已经取消。在并发任务被外部取消时，仍会继续触发消耗极高 CPU 资源的 go build 和 go test 子进程。
- 证据: internal/swarm/planner/pool.go:175-180
  ```go
	buildCtx, cancel1 := context.WithTimeout(ctx, 30*time.Second)
	defer cancel1()

	var compileScore = 0.0

	if p.sandbox != nil {
		_, buildErr := p.sandbox.Execute(buildCtx, "go", []string{"build", tmpDir}, wd, 30*time.Second)
  ```
- 修复方向提示: 在调用 sandbox.Execute 前增加 if ctx.Err() != nil { return } 检查。

## 已审文件清单
- internal/knowledge/chunker.go
- internal/knowledge/chunker_cgo.go
- internal/knowledge/chunker_nocgo.go
- internal/knowledge/conflict.go
- internal/knowledge/connector/extension_librarian_handler.go
- internal/knowledge/connector/mcp_connector.go
- internal/knowledge/connector/notion_connector.go
- internal/knowledge/connector/obsidian_connector.go
- internal/knowledge/connector/registry.go
- internal/knowledge/connector/sync_scheduler.go
- internal/knowledge/graphrag/build.go
- internal/knowledge/graphrag/cluster.go
- internal/knowledge/graphrag/cluster_algorithms.go
- internal/knowledge/graphrag/community_summarizer.go
- internal/knowledge/graphrag/doc_types.go
- internal/knowledge/graphrag/entity.go
- internal/knowledge/graphrag/graph_build_handler.go
- internal/knowledge/graphrag/graph_traverser.go
- internal/knowledge/graphrag/summary_gen_handler.go
- internal/knowledge/graphrag/temporal_view.go
- internal/knowledge/graphrag/writer.go
- internal/knowledge/parsers.go
- internal/knowledge/provider.go
- internal/knowledge/rag.go
- internal/knowledge/rag_impl.go
- internal/knowledge/rag_recent_chunks.go
- internal/knowledge/rag_retrieval.go
- internal/knowledge/rag_summary_tree.go
- internal/knowledge/retriever.go
- internal/knowledge/retriever_parsing.go
- internal/knowledge/source.go
- internal/knowledge/taint_boundary.go
- internal/learning/curriculum/curriculum.go
- internal/learning/curriculum/curriculum_scheduler.go
- internal/learning/curriculum/fitness.go
- internal/learning/curriculum/gap_fill_worker.go
- internal/learning/engine.go
- internal/learning/engine_ops.go
- internal/learning/engine_types.go
- internal/learning/logic_collapse_codegen.go
- internal/learning/logic_collapse_trigger.go
- internal/learning/optimizer/heuristics_store.go
- internal/learning/optimizer/memf.go
- internal/learning/optimizer/memf_heuristics.go
- internal/learning/optimizer/optimizer.go
- internal/learning/optimizer/optimizer_helpers.go
- internal/learning/optimizer/rollout.go
- internal/learning/optimizer/rollout_store.go
- internal/learning/optimizer/version_store.go
- internal/learning/provider.go
- internal/learning/reflexion/bridge.go
- internal/learning/reflexion/reflection_worker.go
- internal/learning/reflexion/reflexion.go
- internal/learning/surprise/drift_detector.go
- internal/learning/surprise/drift_downgrade_registry.go
- internal/learning/surprise/drift_orchestrator.go
- internal/learning/surprise/surprise.go
- internal/learning/surprise/surprise_markov.go
- internal/learning/synthetic/synthetic_eval_gen.go
- internal/learning/synthetic/synthetic_skill_gen.go
- internal/swarm/agents/code_validator.go
- internal/swarm/agents/code_validator_rules.go
- internal/swarm/agents/doc.go
- internal/swarm/agents/governance_agent.go
- internal/swarm/agents/memory_agent.go
- internal/swarm/agents/security_audit_agent.go
- internal/swarm/planner/decomposer.go
- internal/swarm/planner/pool.go
- internal/swarm/provider.go
- internal/swarm/startup.go
- internal/swarm/supervisor/tree.go

## 明确未覆盖的范围
- 无

## 审了但无发现的模块
- 无
