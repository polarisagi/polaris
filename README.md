# polaris

面向 2026+ 的**开源自托管** AI Agent 系统。严格遵循 Harness Engineering 六条架构不变量构建。单机 8GB 内存可运行，支持 Telegram / Discord / Slack / 飞书等主流第三方接入，终端用户无需修改源码即可通过 Shell Script Hooks 自定义生命周期行为。

## 定位与约束

| 维度 | 内容 |
|------|------|
| 定位 | 开源自托管 AI Agent（2026+） |
| 运行环境 | 单机可运行，消费级笔记本，8GB+ 内存 |
| 底座语言 | Go（编排/服务）+ Rust（性能关键路径） |
| 存储 | 多引擎并存：关系型 + 向量 + 图 + KV + 全文检索 + 事件流 |
| 形态 | 多 Agent 协同：黑板模式 + CAS 原子认领 + supervisor tree |
| 核心能力 | 自学习 / 自进化 / 自增强（无梯度主线 + 条件梯度训练） |
| LLM 池 | Provider-agnostic：`<flash-class>` Provider 模型用于 Budget 池（Tier 0-1），`<reasoning-class>` 模型用于 Reasoning 池（Tier 2-3 复杂推理）。Adapter 已实现 OpenAI / Anthropic / DeepSeek 等主流协议 |

> **默认推荐**：开箱即用配置（`configs/defaults.toml`）使用 DeepSeek V4 系列（Flash + Pro），已在 Tier-0 基线长程测试。任何兼容上述协议的 Provider 可平替——参见 `docs/arch/M01-Inference-Runtime.md §3 Provider Adapter`。

## Harness Engineering 六条不变量

| # | 不变量 | 内涵 |
|---|--------|------|
| 1 | **可观测优先** | 从第 0 行代码起全链路可追溯，Token_Burn_Rate + Surprise_Index 一等公民指标 |
| 2 | **可验证执行** | 禁止概率过滤充当安全边界，安全决策物理/密码学可验证 |
| 3 | **可组合原语** | 最小可复用单元，模块间热路径同步接口 + 冷路径结构化事件通信 |
| 4 | **数据驱动迭代** | Eval Harness 驱动自进化，所有变更通过 CI 门控 |
| 5 | **状态机持有控制流** | Go 确定性状态机持有控制流，LLM 仅做概率性填空 |
| 6 | **State-in-DB** | 所有状态持久化落盘，异步事件解耦跨存储状态变更，崩溃恢复从 EventLog 回放 |

## 架构

### 四层架构 / 13 模块 / 6 代码包

```
┌──────────────────────────────────────────────────────┐
│ L3  Interface & Scheduler (M13) │ Eval Harness (M12) │  ← 治理/接口
├──────────────────────────────────────────────────────┤
│ L2  Orchestrator (M8) │ Self-Improve (M9) │ RAG (M10)│  ← 协同/学习
├──────────────────────────────────────────────────────┤
│ L1  Agent Kernel (M4) │ Memory (M5) │ Skill (M6) │   │  ← 认知核心
│     Tool & Action (M7)                               │
├──────────────────────────────────────────────────────┤
│ L0  Inference (M1) │ Storage (M2) │ Observability    │  ← 基础设施
│     (M3) │ Policy & Safety (M11)                     │
└──────────────────────────────────────────────────────┘
```

### 模块 → 代码包映射

| 代码包 | 模块 | 职责 |
|--------|------|------|
| `pkg/substrate` | M1 Inference · M2 Storage · M3 Observability · M11 Policy & Safety | LLM 路由、多引擎存储、全链路追踪、策略执行 |
| `pkg/cognition` | M4 Kernel · M5 Memory · M6 Skill | 状态机、分层记忆、技能库 |
| `pkg/action` | M7 Tool & Action | 沙箱执行、MCP 双向、工具注册 |
| `pkg/swarm` | M8 Orchestrator · M9 Self-Improve · M10 RAG | 多 Agent 黑板、自进化、知识摄入 |
| `pkg/governance` | M12 Eval Harness | 评测门控、轨迹回放、影子执行 |
| `pkg/edge` | M13 Interface & Scheduler | CLI/API/WebUI、任务调度、HITL |

### 硬件分层

| Tier | RAM | 推理来源 |
|------|-----|---------|
| Tier 0（地板） | 8GB | 全部远程 API |
| Tier 1（甜点） | 16GB | 远程 API + 大并发 |
| Tier 2 | 24GB+ | 远程 API + 多 Agent + 全存储栈 |
| Tier 3 | 64GB+ (Apple Silicon) | 全本地推理 |

## 项目结构

```
polaris/
├── cmd/polaris/              # 入口
├── pkg/
│   ├── substrate/            # L0: inference, storage, observability, policy
│   ├── cognition/            # L1: kernel, memory, skill
│   ├── action/               # L1: tool
│   ├── swarm/                # L2: orchestrator, self_improve, knowledge
│   ├── governance/           # L3: eval
│   └── edge/                 # L3: scheduler
├── internal/                 # 私有共享: protocol, config, errors
├── rust/substrate/           # Rust FFI 性能路径
├── skills/                   # 内置技能
├── policies/                 # Cedar 策略
├── configs/                  # 默认配置
├── docs/arch/                # 架构设计文档
├── go.mod
└── Makefile
```

## 运行

**前置条件**: Go 1.26+, Rust 1.94+

## 数据目录 (Data Directory)

项目的全局工作目录位于用户根目录下的 `~/.polarisagi/polaris/`。系统运行时的所有状态数据（包括数据库 `polaris.db`）、日志文件、运行时缓存等均持久化保存在该目录下。

```bash
# 构建
make build

# 运行
make run

# 测试
make test

# 全量检查
make all
```

## 架构设计文档

详见 `docs/arch/` 目录下的 15 份架构设计文档（1 份全局公共字典 + 1 份总览 + 13 份模块深度选型），覆盖全部 13 个模块的前期研究与技术选型。

## 联系与社区

- **官方网站**: [https://polarisagi.online/](https://polarisagi.online/)
- **作者 / 关注我**: mrlaoliai (全网同名：小红书、抖音、TikTok、X 平台等)
- **联系邮箱**: [polarisagi.online@gmail.com](mailto:polarisagi.online@gmail.com)
