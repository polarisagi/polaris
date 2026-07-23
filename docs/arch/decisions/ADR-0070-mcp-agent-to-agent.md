# ADR-0070: MCP Agent-to-Agent (A2A) 协同架构

## 状态
Proposed (战略方向，未排期)

## 依赖关系
- 依赖 [ADR-0050: Swarm 自订阅 CAS 认领语义]
- 依赖 [ADR-0016: 统一信任扩展模型]
- 依赖 [ADR-0017: 基于 Cedar 的动态门控机制]
- 依赖 [ADR-0018: 细粒度 Taint 污点分析]

## 背景 (Context)
随着系统发展，我们希望外部框架的大模型 Agent 可以通过标准 MCP (Model Context Protocol) 协议将任务投递给 Polaris Swarm，或者由 Polaris 将复杂的子任务委派给外部的 Agent 处理（GD-14-005 规范要求）。这属于典型的 Agent-to-Agent (A2A) 协同场景。

当前我们支持了 MCP 客户端作为工具层（Tool Layer）进行交互，但该交互仅停留在“调用函数”级别，缺乏异步任务的声明、状态跟踪和结果回调等语义。
为了稳定推进内部 Swarm 的演进，并在协议层面清晰界定边界，我们在本阶段决定**暂时不落码实现 A2A 协议**，而是通过本 ADR 确立其核心设计思路，待内部 Swarm 的状态语义和 M7 边界完全锁定后再实施。

## 决策 (Decision)
我们将在未来通过以下方式引入 A2A 支持：

### 1. 入站方向 (Inbound: External Agent -> Polaris)
- 在 `internal/gateway/server` 新增 MCP Server 端点。
- 外部 MCP 客户端通过该端点发起任务调用时，不会直接映射为同步的 LLM 生成请求，而是将请求封装为 `TaskEntry` 并落入 `swarm` 的 `sqlite_blackboard`。
- **鉴权与门控**：入站请求的载荷会附带来源上下文，直接接入现有的 M11 策略体系。污点传播（Taint Tracker）和 Cedar 门控（ADR-0017）会像处理 Channel 消息一样，拦截或监控来自 MCP 端点的任务请求。

### 2. 出站方向 (Outbound: Polaris -> External Agent)
- 当 Polaris 内部 Agent 需要外部协助时，不再使用原有的 M7 MCP 客户端的 `ExecuteTool` 路径。
- 会新增一套“任务委派（Delegation）”接口层。此层将遵循 ADR-0016 定义的信任模型，判断被委派的外部 Agent 的沙盒信任等级，以决定是否能将包含敏感状态（如特定上下文信息）的请求发送出去。

### 3. 边界划分 (Tools vs Tasks)
- **Tool Layer (M7 MCP)**：强调低延迟、强模式一致性、同步响应（如：查询天气、读写文件）。
- **Task Layer (A2A MCP)**：强调异步规划、多步决策、容错重试。这两者的实现将完全隔离，避免混淆导致不可预测的超时或锁死。

## 结果 (Consequences)
**正面影响**：
- 为 Polaris AGI 系统打下了开放的联邦计算基础。
- 保证了内部任务语义不受外部协议强制绑定的影响，具有良好的解耦性。

**负面影响**：
- 引入了额外的异步处理和状态轮询机制，对 MCP 标准本身是一种语义上的过度使用或扩展。
- 短期内用户无法直接开箱即用外部 Agent，只能依赖 Tool 层实现同步的有限对接。
