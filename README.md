# polaris

> An **open-source, self-hosted** AI Agent system for 2026 and beyond. Built strictly on six Harness Engineering architectural invariants. Runs from a 2GB+ VPS to a 64GB workstation; 8GB is the recommended development floor (Tier 0). Supports major third-party platforms out of the box. End users can customize lifecycle behavior via Shell Script Hooks — no source code changes required.

[![License: AGPL v3](https://img.shields.io/badge/License-AGPL_v3-blue.svg)](LICENSE)
[![Go 1.26+](https://img.shields.io/badge/Go-1.26+-00ADD8?logo=go)](go.mod)
[![Rust 1.94+](https://img.shields.io/badge/Rust-1.94+-orange?logo=rust)](rust/)

[中文](README_zh.md) | English

---

## Design Philosophy

| Principle | One-liner |
|-----------|-----------|
| **LLM as Co-Processor** | A deterministic Go FSM owns control flow; LLMs fill structured slots, not free-form orchestration. |
| **Code-as-Calculus (CodeAct)** | Complex tasks compile to verifiable sandbox code, not chains of JSON API calls. Batch execution eliminates context bloat. |
| **Test-Time Compute (TTC)** | SurpriseIndex routes dynamically: System 1 (instant heuristics) → System 1.5 (extended reflection) → System 2 (MCTS + multi-judge). |
| **Physical Safety Boundaries** | 5-level taint tracking + Cedar policy engine + 3-tier sandbox. Probabilistic prompt filters are not security boundaries. |
| **Eval-Gated Evolution** | Every candidate (prompt / skill / config / source patch) must pass 7-stage CI staging before entering production memory — hallucinated improvements cannot be promoted. |

---

## What It Is

| Dimension | Details |
|-----------|---------|
| Type | Open-source self-hosted AI Agent (2026+) |
| Runtime | 2GB+ VPS to 64GB workstation; 8GB recommended (Tier 0 dev floor) |
| Languages | Go (orchestration/service) + Rust (performance-critical paths) |
| Storage | Multi-engine: relational + vector + graph + KV + full-text search + event stream |
| Agent model | Three-role swarm: Supervisor (user-facing) + Librarian (background RAG) + Governance (isolated safety judge); async Blackboard + CAS coordination |
| Execution | CodeAct: tasks expressed as verifiable sandbox code; TTC: SurpriseIndex-driven System 1/1.5/2 routing |
| Core capability | Self-learning / self-evolution / self-improvement (gradient-free primary + conditional gradient training) |
| LLM pool | Provider-agnostic: Flash models for budget pool (Tier 0-1), Reasoning models for complex inference (Tier 2-3). Adapters for OpenAI / Anthropic / DeepSeek / Google / Ollama |

> **Default recommendation**: The out-of-the-box config (`configs/defaults.toml`) uses the DeepSeek V4 series (Flash + Pro), validated in long-running Tier-0 baseline tests. Any provider compatible with the above protocols can be substituted.

---

## Six Harness Engineering Invariants

| # | Invariant | Meaning |
|---|-----------|---------|
| 1 | **Observability First** | Full-chain traceability from line 0. Token_Burn_Rate + Surprise_Index are first-class metrics. |
| 2 | **Verifiable Execution** | Probabilistic filters are not security boundaries. Safety decisions must be physically or cryptographically verifiable. |
| 3 | **Composable Primitives** | Minimal reusable units. Synchronous interfaces on hot paths, structured events on cold paths. |
| 4 | **Data-Driven Iteration** | Self-evolution driven by Eval Harness. All changes gated by CI. |
| 5 | **State Machine Owns Control Flow** | A deterministic Go FSM owns control flow. LLMs are probabilistic co-processors. No `while True: call LLM`. |
| 6 | **State-in-DB** | All state persisted to disk. Async events decouple cross-storage mutations. Crash recovery replays from EventLog. |

---

## Architecture

### 4 Layers / 13 Modules / 25+ internal packages

```
┌──────────────────────────────────────────────────────────────┐
│ L3  Gateway (M13) │ Eval Harness (M12) │ Edge/Scheduler      │  ← Interface / Governance
├──────────────────────────────────────────────────────────────┤
│ L2  Orchestrator (M8) │ Self-Improve (M9) │ RAG (M10)         │  ← Collaboration / Learning
│     Extensions (MCP / Plugin / Skill / Marketplace)           │
├──────────────────────────────────────────────────────────────┤
│ L1  Agent Kernel (M4) │ Memory (M5) │ Skill (M6)             │  ← Cognitive Core
│     Tool & Action (M7)                                        │
├──────────────────────────────────────────────────────────────┤
│ L0  Inference (M1) │ Storage (M2) │ Observability (M3)        │  ← Infrastructure
│     Policy & Safety (M11)                                     │
└──────────────────────────────────────────────────────────────┘
```

### Package Mapping

| Package | Modules | Responsibilities |
|---------|---------|-----------------|
| `internal/llm`, `store`, `observability` | M1 · M2 · M3 · M11 | LLM routing, multi-engine storage, full-chain tracing, policy enforcement, taint propagation |
| `internal/agent`, `memory`, `prompt` | M4 · M5 · M6 | Agent FSM, 4-layer memory system, Wasm skill library |
| `internal/tool`, `sandbox`, `action` | M7 | 3-tier sandbox execution, bidirectional MCP, tool registry, CodeAct |
| `internal/extension` | — | MCP client management, plugin marketplace, extension install flow, Skill Creator |
| `internal/swarm`, `learning`, `knowledge` | M8 · M9 · M10 | Multi-agent blackboard, self-improvement engine, knowledge graph RAG |
| `internal/eval` | M12 | Eval gating, trajectory replay, shadow execution, CI integration |
| `internal/gateway`, `channel`, `cli` | M13 (scheduling) | Task scheduling, HITL gateway, cron triggers |
| *(merged)* | M13 (interface) | HTTP API server (OpenAI-compatible), Web UI, 18 channel adapters |

### Hardware Tiers

| Tier | RAM | Capabilities |
|------|-----|-------------|
| VPS-min | 2GB+ | Remote API + SurrealDB (kv-mem) + core features; some features may degrade under load |
| VPS-stable | 4GB+ | Above, all VPS features (SurrealDB + embedding + STT + L2/L3 Wasm sandbox) stable under concurrent load |
| Tier 0 (dev floor) | 8GB | All VPS features + SurrealDB rocksdb (auto) + GraphRAG + DeepRAG + LogicCollapse + ComputerUseGUI; comfortable headroom |
| Tier 1 (sweet spot) | 16GB | Above + local small-model inference (3B) + WebUI + OTel + Presidio PII |
| Tier 2 | 24GB+ | Above + large local LLM (7–14B) + QLoRA gradient training |
| Tier 3 | 64GB+ (Apple Silicon) | Fully local inference, zero cloud dependency |

---

## Key Features

### Third-Party Platform Support (18 channels)

Telegram · Discord · Slack · Feishu · DingTalk · WeCom · Matrix · Mattermost · Microsoft Teams · Signal · SMS · Email · QQBot · Webhook · Home Assistant · and more

### Security

- **5-level taint propagation** (TaintNone → TaintHigh) across the full chain — external data never enters the instruction zone
- **Cedar policy engine** (via purego FFI, no CGO) for tool call authorization
- **3-tier sandbox** (InProcess / Rust-backed / Container) with risk-based isolation; built-in tools run trusted in-process
- **SafeDialer SSRF protection** — blocks RFC-defined private address ranges (including CGNAT 100.64.0.0/10)
- **KillSwitch 3-phase circuit breaker** — process-level, model-level, session-level stops

### Self-Improvement

- **Gradient-free main path**: Reflexion → Logic Collapse skill distillation → PromptOptimizer → SurpriseIndex adaptation
- **MEMF (Memory of Mistakes & Fallacies)**: failure trajectories feed a persistent graph; future routing avoids known failure modes
- **Eval Harness driven**: trajectory recording → shadow execution → regression detection → CI gating → auto circuit-breaker
- **Auto-Curriculum**: automatically generates training curricula based on SurpriseIndex

### Extensibility

- **MCP (Model Context Protocol)**: Streamable HTTP, supports both MCP client and server roles
- **Plugin Bundle**: multi-component plugin packages (MCP server + skills + automations)
- **Shell Script Hooks**: drop scripts into `~/.polarisagi/polaris/hooks/` — no source changes needed
- **Official Plugin Marketplace**: All default plugins, Wasm skills, and MCP servers are developed and maintained at our official open-source repository: [polarisagi/polaris-plugins-official](https://github.com/polarisagi/polaris-plugins-official). Extensions are dynamically downloaded at runtime.

### OpenAI-Compatible API

The HTTP gateway exposes an OpenAI-compatible API, allowing any OpenAI-compatible client (ChatGPT clients, LangChain, etc.) to connect directly.

---

## Project Structure

```
polaris/
├── cmd/polaris/          # Main entry point (minimal; init logic pushed to internal/cli)
├── configs/              # Embedded default configs (bundled with binary)
├── web/                  # Web UI (Vite build, embedded in binary)
├── rust/substrate/       # Rust FFI — Cedar policy engine + SurrealDB + native sandbox
├── internal/
│   ├── # ── L0 Infrastructure ──────────────────────────────────────
│   ├── llm/              # LLM adapters (OpenAI/Anthropic/DeepSeek/Google/Ollama), STT, TTS
│   ├── store/            # SQLite repositories, BM25 full-text search, audit/event log
│   ├── observability/    # Metrics (TokenBurnRate), hardware probe, distributed tracing
│   ├── security/         # 5-level taint, Cedar policy gate, capability tokens, SSRF guard
│   ├── ffi/              # Rust dylib bridge (purego, zero CGO)
│   ├── # ── L1 Cognitive / Execution ────────────────────────────────
│   ├── agent/            # FSM lifecycle, DAG executor, perception context, PII vault
│   ├── memory/           # 4-layer memory (Working/Episodic/Semantic/Procedural), GraphRAG bridge
│   ├── action/           # CodeAct, LAM streaming, Shell Script Hooks, capability tokens
│   ├── tool/             # Tool registry (PolicyGate 5-stage), builtin tools, sandbox adapter
│   ├── sandbox/          # 3-tier sandbox (InProcess / Rust-Wasm / Container)
│   ├── prompt/           # Prompt template management, optimizer
│   ├── vfs/              # Virtual workspace, filesystem isolation
│   ├── # ── L2 Collaboration / Knowledge ────────────────────────────
│   ├── extension/        # MCP client, plugin system, Wasm skill, marketplace
│   ├── swarm/            # Blackboard + CAS orchestrator, planner, Supervisor Tree
│   ├── learning/         # SurpriseIndex, Reflexion, synthetic eval gen, auto-curriculum
│   ├── knowledge/        # GraphRAG pipeline, Obsidian connector, BM25+vector hybrid RAG
│   ├── # ── L3 Interface / Governance ───────────────────────────────
│   ├── gateway/          # HTTP API server (OpenAI-compatible), SSE log stream
│   ├── channel/          # 18 bidirectional platform adapters (TG/Discord/Slack/…)
│   ├── automation/       # Cron scheduler, HITL approval gateway, workflow engine
│   ├── eval/             # Eval harness, trajectory replay, shadow execution, CI gating
│   ├── sysmgr/           # System info, auto-updater, resource downloader, i18n
│   ├── cli/              # CLI bootstrap and command handling
│   └── # ── Cross-layer contracts ───────────────────────────────────
│       ├── protocol/     # Shared types, interfaces, 29 DDL SQL files (Schema SSoT)
│       ├── config/       # Config loading, compile-time invariants
│       ├── errors/       # Unified error types (no bare error leaks)
│       └── lint/         # CI static scan rules
├── pkg/                  # Zero-business-logic utilities (apperr, types, version)
├── docs/arch/            # Architecture design docs (15 documents)
├── testdata/             # Test fixtures and benchmark golden sets
├── tools/                # Go build tools and codegen scripts
├── go.mod
└── Makefile
```

**Runtime data directory**: `~/.polarisagi/polaris/` — database, logs, hooks, extensions, and caches all live here.

---

## Getting Started

### Prerequisites

- Go 1.26+
- Rust 1.94+ (`cargo` in PATH)
- Git

### One-Line Installation

For users who just want to use Polaris without building from source, you can install the pre-compiled binary via the following one-line scripts.

**Linux / macOS (Terminal):**
```bash
# Standard install
curl -sSL https://raw.githubusercontent.com/polarisagi/polaris/main/scripts/install.sh | bash

# China region proxy (if raw.githubusercontent.com is blocked)
curl -sSL https://ghproxy.net/https://raw.githubusercontent.com/polarisagi/polaris/main/scripts/install.sh | bash
# OR
curl -sSL https://mirror.ghproxy.com/https://raw.githubusercontent.com/polarisagi/polaris/main/scripts/install.sh | bash
```

**Windows (PowerShell run as Administrator):**
```powershell
# Standard install
irm https://raw.githubusercontent.com/polarisagi/polaris/main/scripts/install.ps1 | iex

# China region proxy
irm https://ghproxy.net/https://raw.githubusercontent.com/polarisagi/polaris/main/scripts/install.ps1 | iex
# OR
irm https://mirror.ghproxy.com/https://raw.githubusercontent.com/polarisagi/polaris/main/scripts/install.ps1 | iex
```

### Uninstallation

To completely remove the background service and binary (your data in `~/.polarisagi/polaris` will be kept safe):

**Linux / macOS:**
```bash
curl -sSL https://raw.githubusercontent.com/polarisagi/polaris/main/scripts/uninstall.sh | bash
# Proxy: curl -sSL https://ghproxy.net/https://raw.githubusercontent.com/polarisagi/polaris/main/scripts/uninstall.sh | bash
```

**Windows (PowerShell run as Administrator):**
```powershell
irm https://raw.githubusercontent.com/polarisagi/polaris/main/scripts/uninstall.ps1 | iex
# Proxy: irm https://ghproxy.net/https://raw.githubusercontent.com/polarisagi/polaris/main/scripts/uninstall.ps1 | iex
```

### Build from Source

```bash
# Clone
git clone https://github.com/polarisagi/polaris.git
cd polaris

# Build (Rust FFI → Go binary → Web UI)
make build

# Run
./bin/polaris

# Open Web UI
open http://localhost:28888
```

### Configure a Provider

Edit `configs/defaults.toml`, or create an override file in the runtime data directory:

```bash
# Default: DeepSeek V4 (out-of-the-box, no changes needed)
# Any OpenAI-compatible provider: swap base_url and api_key
```

### Common Make Targets

| Command | Description |
|---------|-------------|
| `make build` | Full build (Rust + Go + UI) |
| `make test` | Run test suite |
| `make lint` | Static analysis |
| `make fmt` | Format code |
| `make docs-sync` | Refresh §TOC line numbers in architecture docs |
| `make all` | tidy + fmt + lint + test + build |

---

## Architecture Docs

The `docs/arch/` directory contains 15 design documents covering all 13 modules:

| Document | Content |
|----------|---------|
| `ARCHITECTURE.md` | System overview, SSoT anchors, module completion status |
| `00-Global-Dictionary.md` | Global concept dictionary, HE-Rules, cross-module contracts |
| `M01` – `M13-bis` | Per-module design, technology choices, implementation status |

---

## Contributing

Please read [CONTRIBUTING.md](CONTRIBUTING.md) first. This project maintains high standards for PR quality and architectural consistency.

To report security vulnerabilities, see [SECURITY.md](SECURITY.md) for the private disclosure channel.

---

## Community

- **Website**: [https://polarisagi.online/](https://polarisagi.online/)
- **Author**: mrlaoliai (same handle everywhere: Xiaohongshu, Douyin, TikTok, X, etc.)
- **Email**: [polarisagi.online@gmail.com](mailto:polarisagi.online@gmail.com)

---

## License

[GNU Affero General Public License v3.0](LICENSE) (AGPL-3.0)
