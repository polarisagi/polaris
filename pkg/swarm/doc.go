// Package swarm 是 L2 协同学习层。
// 涵盖模块:
//   - M8 Multi-Agent Orchestrator (黑板 + CAS 认领、Supervisor Tree、7 种编排模式)
//   - M9 Self-Improvement Engine (三环嵌套进化、MEMF、HeuristicsMemory、Auto-Curriculum)
//   - M10 Knowledge & RAG (层级文档树、HybridRetriever、GraphRAG、增量索引)
//
// 不变量: [HE-Rule-4] 数据驱动迭代, [HE-Rule-5] 状态机持有控制流。
// 依赖: cognition (Agent Kernel/Memory/Skill)、substrate (Storage/Observability)。
package swarm
