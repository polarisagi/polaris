# polaris

> An **open-source, self-hosted** AI Agent system for 2026 and beyond. Built strictly on six Harness Engineering architectural invariants. Runs on a single machine with 8GB RAM. Supports major third-party platforms out of the box. End users can customize lifecycle behavior via Shell Script Hooks — no source code changes required.

[![License: AGPL v3](https://img.shields.io/badge/License-AGPL_v3-blue.svg)](LICENSE)
[![Go 1.26+](https://img.shields.io/badge/Go-1.26+-00ADD8?logo=go)](go.mod)
[![Rust 1.94+](https://img.shields.io/badge/Rust-1.94+-orange?logo=rust)](rust/)

[中文](README_zh.md) | English

---

## What It Is

| Dimension | Details |
|-----------|---------|
| Type | Open-source self-hosted AI Agent (2026+) |
| Runtime | Single machine, consumer laptop, 8GB+ RAM |
| Languages | Go (orchestration/service) + Rust (performance-critical paths) |
| Storage | Multi-engine: relational + vector + graph + KV + full-text search + event stream |
| Agent model | Multi-agent collaboration: Blackboard pattern + CAS atomic claim + Supervisor Tree |
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

### 4 Layers / 13 Modules / 8 Packages

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
| `pkg/substrate` | M1 · M2 · M3 · M11 | LLM routing, multi-engine storage, full-chain tracing, policy enforcement, taint propagation |
| `pkg/cognition` | M4 · M5 · M6 | Agent FSM, 4-layer memory system, Wasm skill library |
| `pkg/action` | M7 | 3-tier sandbox execution, bidirectional MCP, tool registry, CodeAct |
| `pkg/extensions` | — | MCP client management, plugin marketplace, extension install flow, Skill Creator |
| `pkg/swarm` | M8 · M9 · M10 | Multi-agent blackboard, self-improvement engine, knowledge graph RAG |
| `pkg/governance` | M12 | Eval gating, trajectory replay, shadow execution, CI integration |
| `pkg/edge` | M13 (scheduling) | Task scheduling, HITL gateway, cron triggers |
| `pkg/gateway` | M13 (interface) | HTTP API server (OpenAI-compatible), Web UI, 18 channel adapters |

### Hardware Tiers

| Tier | RAM | Capabilities |
|------|-----|-------------|
| Tier 0 (floor) | 8GB | All-remote API, single agent |
| Tier 1 (sweet spot) | 16GB | Remote API + high concurrency + local embedding |
| Tier 2 | 24GB+ | Remote API + multi-agent + full storage stack |
| Tier 3 | 64GB+ (Apple Silicon) | Fully local inference, zero cloud dependency |

---

## Key Features

### Third-Party Platform Support (18 channels)

Telegram · Discord · Slack · Feishu · DingTalk · WeCom · Matrix · Mattermost · Microsoft Teams · Signal · SMS · Email · QQBot · Webhook · Home Assistant · and more

### Security

- **5-level taint propagation** (TaintNone → TaintHigh) across the full chain — external data never enters the instruction zone
- **Cedar policy engine** (via purego FFI, no CGO) for tool call authorization
- **3-tier sandbox** (InProcess / Wasm-wazero / Container) with risk-based isolation
- **SafeDialer SSRF protection** — blocks RFC-defined private address ranges (including CGNAT 100.64.0.0/10)
- **KillSwitch 3-phase circuit breaker** — process-level, model-level, session-level stops

### Self-Improvement

- **Gradient-free main path**: Reflexion → Logic Collapse skill distillation → PromptOptimizer → SurpriseIndex adaptation
- **Eval Harness driven**: trajectory recording → shadow execution → regression detection → CI gating → auto circuit-breaker
- **Auto-Curriculum**: automatically generates training curricula based on SurpriseIndex

### Extensibility

- **MCP (Model Context Protocol)**: Streamable HTTP, supports both MCP client and server roles
- **Plugin Bundle**: multi-component plugin packages (MCP server + skills + automations)
- **Shell Script Hooks**: drop scripts into `~/.polarisagi/polaris/hooks/` — no source changes needed

### OpenAI-Compatible API

The HTTP gateway exposes an OpenAI-compatible API, allowing any OpenAI-compatible client (ChatGPT clients, LangChain, etc.) to connect directly.

---

## Project Structure

```
polaris/
├── cmd/polaris/          # Main entry point (CLI)
├── pkg/
│   ├── substrate/        # L0: inference, storage, observability, policy
│   ├── cognition/        # L1: kernel, memory, skill
│   ├── action/           # L1: tool, sandbox, MCP action
│   ├── extensions/       # L2: MCP manager, marketplace, plugin, skill creator
│   ├── swarm/            # L2: orchestrator, self_improve, knowledge
│   ├── governance/       # L3: eval harness
│   ├── edge/             # L3: scheduler, HITL
│   └── gateway/          # L3: HTTP server, 18 channel adapters, Web UI
├── internal/             # Private shared: protocol, config, errors
├── rust/substrate/       # Rust FFI performance paths (Cedar, embedded inference)
├── skills/               # Built-in Wasm skill sources (compiled by make build-skills)
├── plugins/builtin/      # Built-in plugin bundles
├── policies/             # Cedar policy files (hard_constraints / soft_constraints)
├── configs/              # Embedded default configs (bundled with binary)
├── web/                  # Web UI (Vite build, embedded in binary)
├── docs/arch/            # Architecture design docs (15 documents)
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
open http://localhost:8080
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
