# polaris

> 面向 2026+ 的**开源自托管** AI Agent 系统。严格遵循 Harness Engineering 六条架构不变量构建。单机 8GB 内存可运行，支持多种第三方平台接入，终端用户无需修改源码即可通过 Shell Script Hooks 自定义生命周期行为。

[![License: AGPL v3](https://img.shields.io/badge/License-AGPL_v3-blue.svg)](LICENSE)
[![Go 1.26+](https://img.shields.io/badge/Go-1.26+-00ADD8?logo=go)](go.mod)
[![Rust 1.94+](https://img.shields.io/badge/Rust-1.94+-orange?logo=rust)](rust/)

[English](README_EN.md) | 中文

---

## 定位与约束

| 维度 | 内容 |
|------|------|
| 定位 | 开源自托管 AI Agent（2026+） |
| 运行环境 | 单机可运行，消费级笔记本，8GB+ 内存 |
| 底座语言 | Go（编排/服务）+ Rust（性能关键路径） |
| 存储 | 多引擎并存：关系型 + 向量 + 图 + KV + 全文检索 + 事件流 |
| 形态 | 多 Agent 协同：黑板模式 + CAS 原子认领 + Supervisor Tree |
| 核心能力 | 自学习 / 自进化 / 自增强（无梯度主线 + 条件梯度训练） |
| LLM 池 | Provider-agnostic：Flash 模型用于预算池（Tier 0-1），Reasoning 模型用于复杂推理（Tier 2-3）。支持 OpenAI / Anthropic / DeepSeek / Google / Ollama 协议 |

> **默认推荐**：开箱即用配置（`configs/defaults.toml`）使用 DeepSeek V4 系列（Flash + Pro），已在 Tier-0 基线长程测试。任何兼容上述协议的 Provider 均可替换。

---

## Harness Engineering 六条不变量

| # | 不变量 | 内涵 |
|---|--------|------|
| 1 | **可观测优先** | 从第 0 行代码起全链路可追溯，Token_Burn_Rate + Surprise_Index 一等公民指标 |
| 2 | **可验证执行** | 禁止概率过滤充当安全边界，安全决策物理/密码学可验证 |
| 3 | **可组合原语** | 最小可复用单元，模块间热路径同步接口 + 冷路径结构化事件通信 |
| 4 | **数据驱动迭代** | Eval Harness 驱动自进化，所有变更通过 CI 门控 |
| 5 | **状态机持有控制流** | Go 确定性状态机持有控制流，LLM 仅做概率性填空 |
| 6 | **State-in-DB** | 所有状态持久化落盘，异步事件解耦跨存储状态变更，崩溃恢复从 EventLog 回放 |

---

## 架构

### 四层架构 / 13 模块 / 8 代码包

```
┌──────────────────────────────────────────────────────────────┐
│ L3  Gateway (M13) │ Eval Harness (M12) │ Edge/Scheduler      │  ← 接口/治理
├──────────────────────────────────────────────────────────────┤
│ L2  Orchestrator (M8) │ Self-Improve (M9) │ RAG (M10)         │  ← 协同/学习
│     Extensions (MCP/Plugin/Skill/Marketplace)                 │
├──────────────────────────────────────────────────────────────┤
│ L1  Agent Kernel (M4) │ Memory (M5) │ Skill (M6)             │  ← 认知核心
│     Tool & Action (M7)                                        │
├──────────────────────────────────────────────────────────────┤
│ L0  Inference (M1) │ Storage (M2) │ Observability (M3)        │  ← 基础设施
│     Policy & Safety (M11)                                     │
└──────────────────────────────────────────────────────────────┘
```

### 代码包映射

| 代码包 | 覆盖模块 | 职责 |
|--------|----------|------|
| `pkg/substrate` | M1 · M2 · M3 · M11 | LLM 路由、多引擎存储、全链路追踪、策略执行、Taint 传播 |
| `pkg/cognition` | M4 · M5 · M6 | Agent 状态机、分层记忆（四层）、技能库（Wasm） |
| `pkg/action` | M7 | 三级沙箱执行、MCP 双向、工具注册、CodeAct |
| `pkg/extensions` | — | MCP 客户端管理、插件市场、扩展安装流、Skill Creator |
| `pkg/swarm` | M8 · M9 · M10 | 多 Agent 黑板、自进化引擎、知识图谱 RAG |
| `pkg/governance` | M12 | 评测门控、轨迹回放、影子执行、Eval Harness |
| `pkg/edge` | M13（调度） | 任务调度、HITL 网关、Cron 触发 |
| `pkg/gateway` | M13（接口） | HTTP API Server（OpenAI 兼容）、Web UI、18 渠道适配 |

### 硬件分层

| Tier | RAM | 能力 |
|------|-----|------|
| Tier 0（地板） | 8GB | 全部远程 API，单 Agent |
| Tier 1（甜点） | 16GB | 远程 API + 高并发 + 本地 Embedding |
| Tier 2 | 24GB+ | 远程 API + 多 Agent + 全存储栈 |
| Tier 3 | 64GB+（Apple Silicon） | 全本地推理，零云端依赖 |

---

## 核心特性

### 第三方平台接入（18 渠道）

Telegram · Discord · Slack · 飞书 · 钉钉 · 企业微信 · Matrix · Mattermost · Microsoft Teams · Signal · SMS · Email · QQBot · Webhook · Home Assistant · 及更多

### 安全体系

- **五级污点传播**（TaintNone → TaintHigh）贯穿全链路，外部数据不进指令区
- **Cedar 策略引擎**（purego FFI，无 CGO）用于工具调用授权
- **三级沙箱**（InProcess / Wasm-wazero / Container）按风险等级隔离执行
- **SafeDialer SSRF 防护**，屏蔽内网 RFC 地址段
- **KillSwitch 三阶段熔断**，支持进程级、模型级、会话级停止

### 自进化能力

- **无梯度主线**：Reflexion 反思 → Logic Collapse 技能蒸馏 → PromptOptimizer 提示优化 → SurpriseIndex 自适应
- **Eval Harness 驱动**：轨迹录制 → 影子执行 → 回归检测 → CI 门控 → 自动熔断
- **Auto-Curriculum**：基于 SurpriseIndex 自动生成训练课程

### 可扩展性

- **MCP（Model Context Protocol）**：Streamable HTTP，支持作为 MCP 客户端和服务端
- **Plugin Bundle**：多组件插件包，含 MCP 服务 + 技能 + 自动化
- **Shell Script Hooks**：`~/.polarisagi/polaris/hooks/` 目录，无需改源码定制生命周期

---

## 项目结构

```
polaris/
├── cmd/polaris/          # 主入口（CLI）
├── pkg/
│   ├── substrate/        # L0: inference, storage, observability, policy
│   ├── cognition/        # L1: kernel, memory, skill
│   ├── action/           # L1: tool, sandbox, MCP action
│   ├── extensions/       # L2: MCP manager, marketplace, plugin, skill creator
│   ├── swarm/            # L2: orchestrator, self_improve, knowledge
│   ├── governance/       # L3: eval harness
│   ├── edge/             # L3: scheduler, HITL
│   └── gateway/          # L3: HTTP server, channels (18 adapters), Web UI
├── internal/             # 私有共享：protocol, config, errors
├── rust/substrate/       # Rust FFI 性能路径（Cedar，嵌入式推理）
├── skills/               # 内置 Wasm 技能源码（make build-skills 编译）
├── plugins/builtin/      # 内置插件包
├── policies/             # Cedar 策略文件（hard_constraints / soft_constraints）
├── configs/              # 嵌入式默认配置（随二进制打包）
├── web/                  # Web UI（Vite 构建，嵌入二进制）
├── docs/arch/            # 架构设计文档（15 份）
├── go.mod
└── Makefile
```

**运行时数据目录**：`~/.polarisagi/polaris/`（数据库、日志、Hooks、扩展、缓存等均在此目录）

---

## 快速开始

### 前置条件

- Go 1.26+
- Rust 1.94+（`cargo` 在 PATH 中）
- Git

### 构建与运行

```bash
# 克隆
git clone https://github.com/polarisagi/polaris.git
cd polaris

# 构建（Rust FFI → Go 二进制 → Web UI）
make build

# 运行
./bin/polaris

# 打开 Web UI
open http://localhost:8080
```

### 配置 Provider

编辑 `configs/defaults.toml` 或在运行时数据目录创建覆盖文件：

```bash
# 推荐：DeepSeek V4（默认，开箱即用）
# 其他兼容 OpenAI 协议的 Provider 直接替换 base_url 和 api_key
```

### 常用 Make 命令

| 命令 | 说明 |
|------|------|
| `make build` | 完整构建（Rust + Go + UI） |
| `make test` | 运行测试套件 |
| `make lint` | 代码静态检查 |
| `make fmt` | 格式化代码 |
| `make docs-sync` | 刷新架构文档 §跳读 行号索引 |
| `make all` | tidy + fmt + lint + test + build |

---

## 架构设计文档

`docs/arch/` 目录下包含 15 份架构设计文档，覆盖全部 13 个模块：

| 文档 | 内容 |
|------|------|
| `ARCHITECTURE.md` | 系统总览、SSoT 锚点、模块完成度 |
| `00-Global-Dictionary.md` | 全局概念字典、HE-Rules、跨模块规则 |
| `M01` ~ `M13-bis` | 13 个模块的设计、选型、实现状态 |

---

## 贡献

请先阅读 [CONTRIBUTING.md](CONTRIBUTING.md)。本项目对 PR 质量与架构一致性有较高要求。

安全漏洞请参阅 [SECURITY.md](SECURITY.md) 通过私信渠道上报。

---

## 联系与社区

- **官方网站**: [https://polarisagi.online/](https://polarisagi.online/)
- **作者 / 关注我**: mrlaoliai（全网同名：小红书、抖音、TikTok、X 平台等）
- **联系邮箱**: [polarisagi.online@gmail.com](mailto:polarisagi.online@gmail.com)

---

## 许可证

[GNU Affero General Public License v3.0](LICENSE) (AGPL-3.0)
