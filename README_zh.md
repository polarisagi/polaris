# polaris

> 面向 2026+ 的**开源自托管** AI Agent 系统。严格遵循 Harness Engineering 六条架构不变量构建。2GB+ VPS 即可运行，8GB 为推荐开发地板（Tier 0）；支持多种第三方平台接入，终端用户无需修改源码即可通过 Shell Script Hooks 自定义生命周期行为。

[![License: AGPL v3](https://img.shields.io/badge/License-AGPL_v3-blue.svg)](LICENSE)
[![Go 1.26+](https://img.shields.io/badge/Go-1.26+-00ADD8?logo=go)](go.mod)
[![Rust 1.94+](https://img.shields.io/badge/Rust-1.94+-orange?logo=rust)](rust/)

[English](README.md) | 中文

---

## 设计哲学

| 原则 | 一句话 |
|------|--------|
| **大模型是协处理器** | Go 确定性状态机持有控制流，LLM 做概率性填空，不做自由编排。 |
| **代码即演算（CodeAct）** | 复杂任务编译为可验证的沙箱代码，而非 JSON API 调用链；批量执行消除上下文膨胀。 |
| **测试时算力（TTC）** | SurpriseIndex 驱动三档动态路由：System 1（启发即答）→ System 1.5（扩展反思）→ System 2（MCTS + 多裁判）。 |
| **物理安全边界** | 五级 Taint 传播 + Cedar 策略引擎 + 三级沙箱；概率过滤不是安全边界。 |
| **Eval 门控进化** | 所有候选（Prompt / Skill / Config / 源码补丁）必须通过 Staging 七阶段 CI 才能写入生产记忆——幻觉改进无法晋升。 |

---

## 定位与约束

| 维度 | 内容 |
|------|------|
| 定位 | 开源自托管 AI Agent（2026+） |
| 运行环境 | 2GB+ VPS 至 64GB 工作站均可运行；8GB 为推荐开发地板（Tier 0） |
| 底座语言 | Go（编排/服务）+ Rust（性能关键路径） |
| 存储 | 多引擎并存：关系型 + 向量 + 图 + KV + 全文检索 + 事件流 |
| 形态 | 三角色 Swarm：Supervisor（用户侧执行）+ Librarian（后台 RAG）+ Governance（隔离安全裁判）；异步黑板 + CAS 协调 |
| 执行模型 | CodeAct：任务编译为可验证沙箱代码；TTC：SurpriseIndex 驱动 System 1/1.5/2 路由 |
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

### 四层架构 / 13 模块 / 25+ 内部包

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
| `internal/llm`, `store`, `observability` | M1 · M2 · M3 · M11 | LLM 路由、多引擎存储、全链路追踪、策略执行、Taint 传播 |
| `internal/agent`, `memory`, `prompt` | M4 · M5 · M6 | Agent 状态机、分层记忆（四层）、技能库（Wasm） |
| `internal/tool`, `sandbox`, `action` | M7 | 三级沙箱执行、MCP 双向、工具注册、CodeAct |
| `internal/extension` | — | MCP 客户端管理、插件市场、扩展安装流、Skill Creator |
| `internal/swarm`, `learning`, `knowledge` | M8 · M9 · M10 | 多 Agent 黑板、自进化引擎、知识图谱 RAG |
| `internal/eval` | M12 | 评测门控、轨迹回放、影子执行、Eval Harness |
| `internal/gateway`, `channel`, `cli` | M13（调度） | 任务调度、HITL 网关、Cron 触发 |
| *(merged)* | M13（接口） | HTTP API Server（OpenAI 兼容）、Web UI、18 渠道适配 |

### 硬件分层

| Tier | RAM | 能力 |
|------|-----|------|
| VPS-min | 2GB+ | 远程 API + SurrealDB(kv-mem) + 核心功能；高并发负载下部分特性可能触发降级 |
| VPS-stable | 4GB+ | 以上 + SurrealDB / Embedding / STT / L2+L3 Wasm 沙箱可稳定并发，不触发 `DegradeMemoryMB` |
| Tier 0（开发地板） | 8GB | 所有 VPS 特性 + SurrealDB rocksdb（自动）+ GraphRAG + DeepRAG + LogicCollapse + ComputerUseGUI；内存充裕，无压力振荡 |
| Tier 1（甜点） | 16GB | 以上 + 本地小模型推理(3B) + WebUI + OTel + Presidio PII |
| Tier 2 | 24GB+ | 以上 + 大型本地 LLM(7-14B) + QLoRA 梯度训练 |
| Tier 3 | 64GB+（Apple Silicon） | 全本地推理，零云端依赖 |

---

## 核心特性

### 第三方平台接入（18 渠道）

Telegram · Discord · Slack · 飞书 · 钉钉 · 企业微信 · Matrix · Mattermost · Microsoft Teams · Signal · SMS · Email · QQBot · Webhook · Home Assistant · 及更多

### 安全体系

- **五级污点传播**（TaintNone → TaintHigh）贯穿全链路，外部数据不进指令区
- **Cedar 策略引擎**（purego FFI，无 CGO）用于工具调用授权
- **三级沙箱**（InProcess / Rust / Container）按风险等级隔离；内置工具直接信任，不走沙箱
- **SafeDialer SSRF 防护**，屏蔽内网 RFC 地址段
- **KillSwitch 三阶段熔断**，支持进程级、模型级、会话级停止

### 自进化能力

- **无梯度主线**：Reflexion 反思 → Logic Collapse 技能蒸馏 → PromptOptimizer 提示优化 → SurpriseIndex 自适应
- **MEMF（错误与谬误记忆）**：失败轨迹提炼为持久化图谱，未来路由规避已知失败模式
- **Eval Harness 驱动**：轨迹录制 → 影子执行 → 回归检测 → CI 门控 → 自动熔断
- **Auto-Curriculum**：基于 SurpriseIndex 自动生成训练课程

### 可扩展性

- **MCP（Model Context Protocol）**：Streamable HTTP，支持作为 MCP 客户端和服务端
- **Plugin Bundle**：多组件插件包，含 MCP 服务 + 技能 + 自动化
- **Shell Script Hooks**：`~/.polarisagi/polaris/hooks/` 目录，无需改源码定制生命周期
- **官方插件市场**：所有的默认插件、Wasm 技能和 MCP 服务均在我们的官方开源仓库 [polarisagi/polaris-plugins-official](https://github.com/polarisagi/polaris-plugins-official) 中独立维护。Polaris 主引擎会在运行时动态从该市场下载分发。

---

## 项目结构

```
polaris/
├── cmd/polaris/          # 主入口（极简，初始化逻辑下推至 internal/cli）
├── configs/              # 嵌入式默认配置（随二进制打包）
├── web/                  # Web UI（Vite 构建，嵌入二进制）
├── rust/substrate/       # Rust FFI — Cedar 策略引擎 + SurrealDB + 原生进程沙箱
├── internal/
│   ├── # ── L0 基础设施 ──────────────────────────────────────────────
│   ├── llm/              # LLM 适配器（OpenAI/Anthropic/DeepSeek/Google/Ollama）、STT、TTS
│   ├── store/            # SQLite Repository、BM25 全文检索、审计/事件日志
│   ├── observability/    # 指标（TokenBurnRate）、硬件探针、分布式追踪
│   ├── security/         # 五级污点传播、Cedar 策略门、能力令牌、SSRF 防护
│   ├── ffi/              # Rust dylib 桥接（purego，零 CGO）
│   ├── # ── L1 认知/执行 ─────────────────────────────────────────────
│   ├── agent/            # FSM 生命周期、DAG 执行器、感知上下文、PII Vault
│   ├── memory/           # 四层记忆（Working/Episodic/Semantic/Procedural）、图谱桥
│   ├── action/           # CodeAct、LAM 流式、Shell Script Hooks、能力令牌
│   ├── tool/             # 工具注册（PolicyGate 五阶段）、内置工具集、沙箱适配
│   ├── sandbox/          # 三级沙箱（InProcess / Rust-Wasm / Container）
│   ├── prompt/           # 提示词模板管理、优化器
│   ├── vfs/              # 虚拟工作区、文件系统隔离
│   ├── # ── L2 协同/知识 ─────────────────────────────────────────────
│   ├── extension/        # MCP 客户端、插件系统、Wasm 技能、插件市场
│   ├── swarm/            # 黑板 + CAS 编排器、规划器、Supervisor Tree
│   ├── learning/         # SurpriseIndex、Reflexion、合成评测生成、自动课程
│   ├── knowledge/        # GraphRAG 管线、Obsidian 连接器、BM25+向量混合检索
│   ├── # ── L3 接口/治理 ─────────────────────────────────────────────
│   ├── gateway/          # HTTP API 网关（OpenAI 兼容）、SSE 日志流
│   ├── channel/          # 18 渠道双向适配器（TG/Discord/Slack/飞书/钉钉/…）
│   ├── automation/       # Cron 调度、HITL 人工审批网关、自动化工作流
│   ├── eval/             # 评测执行器、轨迹回放、影子执行、CI 门控
│   ├── sysmgr/           # 系统信息、自动更新、资源下载、本地化
│   ├── cli/              # 命令行引导与命令处理
│   └── # ── 跨层契约 ──────────────────────────────────────────────────
│       ├── protocol/     # 共享类型、接口契约、29 个 DDL SQL 文件（Schema SSoT）
│       ├── config/       # 配置加载、编译期不变量
│       ├── errors/       # 统一错误类型（禁裸 error 泄漏）
│       └── lint/         # CI 静态扫描规则
├── pkg/                  # 零业务逻辑工具包（apperr、types、version）
├── docs/arch/            # 架构设计文档（15 份）
├── testdata/             # 测试夹具和基准 golden 集
├── tools/                # Go 构建工具和代码生成脚本
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

### 一键安装

如果你只想使用 Polaris 而不需要修改源码，可以通过以下一键脚本安装编译好的最新二进制版本，并自动配置开机后台自启。

**Linux / macOS (终端):**
```bash
# 标准安装
curl -sSL https://raw.githubusercontent.com/polarisagi/polaris/main/scripts/install.sh | bash

# 中国大陆网络代理（推荐，如果直连失败）
curl -sSL https://ghproxy.net/https://raw.githubusercontent.com/polarisagi/polaris/main/scripts/install.sh | bash
# 或使用备用代理
curl -sSL https://mirror.ghproxy.com/https://raw.githubusercontent.com/polarisagi/polaris/main/scripts/install.sh | bash
```

**Windows (以管理员身份运行 PowerShell):**
```powershell
# 标准安装
irm https://raw.githubusercontent.com/polarisagi/polaris/main/scripts/install.ps1 | iex

# 中国大陆网络代理（推荐，如果直连失败）
irm https://ghproxy.net/https://raw.githubusercontent.com/polarisagi/polaris/main/scripts/install.ps1 | iex
# 或使用备用代理
irm https://mirror.ghproxy.com/https://raw.githubusercontent.com/polarisagi/polaris/main/scripts/install.ps1 | iex
```

### 卸载

彻底移除后台服务和二进制文件（你在 `~/.polarisagi/polaris` 下的数据库和配置数据会被安全保留）：

**Linux / macOS:**
```bash
curl -sSL https://raw.githubusercontent.com/polarisagi/polaris/main/scripts/uninstall.sh | bash
# 代理方式: curl -sSL https://ghproxy.net/https://raw.githubusercontent.com/polarisagi/polaris/main/scripts/uninstall.sh | bash
```

**Windows (以管理员身份运行 PowerShell):**
```powershell
irm https://raw.githubusercontent.com/polarisagi/polaris/main/scripts/uninstall.ps1 | iex
# 代理方式: irm https://ghproxy.net/https://raw.githubusercontent.com/polarisagi/polaris/main/scripts/uninstall.ps1 | iex
```

### 源码构建与运行

```bash
# 克隆
git clone https://github.com/polarisagi/polaris.git
cd polaris

# 构建（Rust FFI → Go 二进制 → Web UI）
make build

# 运行
./bin/polaris

# 打开 Web UI
open http://localhost:28888
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
