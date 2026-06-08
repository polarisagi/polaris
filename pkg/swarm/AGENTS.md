# pkg/swarm/ (L2 协同学习: M8 编排/M9 自进化/M10 RAG)

> Canonical arch docs: [M08-Multi-Agent-Orchestrator.md](../../docs/arch/M08-Multi-Agent-Orchestrator.md) · [M09-Self-Improvement-Engine.md](../../docs/arch/M09-Self-Improvement-Engine.md) · [M10-Knowledge-RAG.md](../../docs/arch/M10-Knowledge-RAG.md)

**硬约束**:
1. Blackboard: 认领必 CAS 原子; Lease 60s, 心跳 15s, Reaper 1s 扫描
2. 7 阶段 Staging: M9/M6 输出合并前必走完 7 阶段流水线 (失败→拒绝/回滚)
3. LLM-as-Judge (XR-05): 自进化输出必经异构模型审查
4. 底层共享: M10(RAG) 与 M5(Memory) 共用 hybrid_retrieve, 配置独立单实例
5. 依赖单向: 禁 import pkg/{governance,edge,gateway}

**高频陷阱**:
- SurpriseIndex: 三路加权 (Embedding 0.4 + ToolSeq 0.35 + MEMF 0.25), M9 推送至 M3, 禁直读 M4 缓存
- SurpriseIndex 路由阈值: low<0.30→System1, high>0.60→System2 (可配置覆盖)
- Layer B 自动激活: 需 500+ 次成功转移 (DefaultLayerBThreshold=1000 次转移事件)
- Rollout 推进: M9 决策, M13 执行, M9 禁自行切流
- Memory 写入必经 Cedar 门控
- 安全一票否决: newly_failing safety=regress, 无例外
- BFS 图参数硬卡: depth=2, maxNeighbors=20, nodes=200

**文件索引**:
- [标杆] `blackboard.go`: Blackboard 接口 + TaskEntry 定义
- [标杆] `sqlite_blackboard.go`: SQLiteBlackboard 持久化后端
- [标杆] `self_improve/engine.go`: M9 三环架构
- [标杆] `knowledge/rag_impl.go`: M10 摄入管线
- [参照] `orchestrator.go`: Orchestrator (多 Agent 调度, Tier 0 上限 maxAgents=3)
- [参照] `reaper.go`: Reaper 孤儿任务回收 (Phase1 扫租约 / Phase2 GC)
- [参照] `surprise.go`: SurpriseIndex 计算 + System1/2 路由
- [参照] `memf.go`: FallacyMemoryPool + HeuristicsMemory (MEMF)
- [参照] `graph_build.go`: 知识图谱增量构建
- [参照] `prompt_optimizer.go`: GEPA/MemAPO 优化
- [参照] `reflexion.go`: M9 内环反思
- [参照] `curriculum.go`: M9 中环边缘任务发现
- [参照] `rollout.go`: M9 外环阶梯
- [参照] `supervisor/tree.go`: 监督树

**跨模块**:
- L1 通信与 L3 暴露均走 `internal/protocol/`
- 改 Blackboard / Staging 步骤 → B5 `[proto-break]`
