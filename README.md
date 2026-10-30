# Polaris

> **Your own private AI assistant** — installed on your computer or server, with 100% of your data kept locally. Organize files, write code, research, and automate everyday work in plain language.

[![License: AGPL v3](https://img.shields.io/badge/License-AGPL_v3-blue.svg)](LICENSE)
[![Website](https://img.shields.io/badge/Website-polarisagi.online-00ADD8)](https://polarisagi.online/)

**English** | [中文](README_zh.md)

---

## What is Polaris?

Most AI tools today either run on someone else's cloud (leaving your private conversations, documents, and data on third-party servers) or act as a mere chat window — able to talk, but unable to actually get work done for you.

**Polaris is an open-source, self-hosted personal AI assistant built to take action.**

Install it with a single command on your Mac, Windows PC, Linux machine, or personal server. Tell it what you need in everyday language — organize local files, research and summarize documents, write and run code, track daily updates, or automate repetitive workflows. **Every conversation, memory, and file stays on your own device and never passes through a third-party relay server.**

---

## Why Polaris?

### 1. Gets Real Work Done — Without Going Off the Rails

Many AI assistants lose track, get stuck in loops, or hallucinate halfway through a multi-step task. Polaris is built around a rock-solid task execution engine: when given a complex goal, it breaks the work into clear steps, verifies each result along the way, and automatically rolls back and retries if something goes wrong — reliably seeing tasks through to completion.

### 2. Instant on Simple Tasks, Deep on Hard Ones

Not every request needs the slowest, most expensive AI model. Polaris automatically gauges how familiar or complex a task is:
- **Everyday routines**: handled immediately via reusable local skills with millisecond response and zero model cost;
- **Moderate requests**: answered quickly with lightweight reasoning;
- **Complex, unfamiliar challenges**: automatically routed to deep reasoning and multi-path exploration for high-quality results.

### 3. A Built-In Team of Three Specialists

Behind the scenes, Polaris doesn't rely on a single overburdened AI. Instead, three dedicated roles work together for you:
- **Supervisor (Main Assistant)**: talks with you directly, understands your goals, and coordinates the task from start to finish;
- **Librarian (Researcher)**: works quietly in the background, searching your documents, knowledge base, and past history to supply the right context at the right moment;
- **Governance (Safety Officer)**: operates independently to review actions before they run, blocking risky or unintended operations.

### 4. Four-Layer Long-Term Memory That Grows With You

Most AI tools forget who you are the moment you close the window. Polaris models human memory across four distinct layers:
- **Working memory**: keeps track of what you are doing right now;
- **Episodic memory**: remembers your full history of conversations and events, searchable across sessions;
- **Semantic memory**: builds a personal knowledge graph of your preferences, domain concepts, and relationships;
- **Procedural memory**: saves proven workflows as reusable skills so future tasks run effortlessly.

The longer you use Polaris, the better it understands your habits, preferences, and work style.

### 5. Privacy and Safety Locked In by Design

- **100% Data Ownership**: all data lives locally in `~/.polaris/` under your full control;
- **Isolated Sandbox Execution**: scripts and tools run inside an isolated safety sandbox so they cannot harm your system;
- **Protection Against Web Prompt Injection**: untrusted web pages and external data are strictly separated from your instructions, preventing malicious content from hijacking your assistant;
- **Clear Permission Modes**: choose between Read-Only mode, Ask-Before-Acting approval, or Full Auto mode — keeping you in charge of sensitive actions.

### 6. Learns From Experience — Never Repeats the Same Mistake

Polaris distills lessons from both successes and failures. When it encounters a situation that previously caused an error, it recognizes the trap early and steers clear. Every self-improvement is tested and verified before adoption, ensuring your assistant steadily improves over time.

---

## Key Features at a Glance

- 🖥️ **Desktop App + Web Browser UI**: comes with both a lightweight native desktop app (for macOS, Windows, and Linux — system tray, auto-start, native notifications) and a clean browser interface.
- 🎙️ **Natural Voice Interaction**: supports offline voice input (speech-to-text) and natural voice read-aloud (text-to-speech). Voice models are downloaded on demand and unloaded when idle.
- 💬 **Connect Your Favorite Messaging Apps (18+ Channels)**: link Polaris with Telegram, Discord, Slack, Feishu/Lark, DingTalk, WeCom, Microsoft Teams, Matrix, Mattermost, Signal, Email, Webhooks, Home Assistant, and more — message your home or office Polaris from your phone anywhere.
- ⏰ **Scheduled Tasks & Automation**: set up recurring jobs in plain language (daily briefings, file organization, periodic checks) with optional human-in-the-loop approval for important actions.
- 🧩 **Official Plugin Marketplace & Easy Extensions**: install skills and MCP tools from the [Official Open-Source Plugin Marketplace](https://github.com/polarisagi/polaris-plugins-official), or drop custom scripts into `~/.polaris/hooks/`.
- 🤝 **Multi-Computer Collaboration**: pair multiple computers running Polaris over LAN or Tailscale so they can delegate tasks to one another, or interoperate with other agents via the standard A2A protocol.
- 🔌 **Works With Your Choice of AI Models (Cloud or Local)**: recommends cost-effective DeepSeek out of the box, supports OpenAI, Anthropic (Claude), Google (Gemini), or any compatible provider, and runs 100% offline local models via Ollama. Also provides an OpenAI-compatible API endpoint.

---

## System Requirements

Polaris is self-contained and requires no external databases or complex setup:

- **Supported OS**: macOS (Apple Silicon & Intel), Windows (x64 & ARM64), Linux (x86_64 & ARM64)
- **Minimum**: 2 GB RAM (runs core features even on an entry-level cloud VPS)
- **Recommended**: 8 GB+ RAM (any modern laptop, desktop, or Mac mini runs the full feature set smoothly; 16 GB+ RAM is recommended for running offline local models)

---

## One-Line Install

Copy and paste a single command below to download, install, and configure Polaris to start automatically.

### macOS / Linux

Run in your Terminal:

```bash
# Standard install
curl -fsSL https://raw.githubusercontent.com/polarisagi/polaris/main/scripts/install.sh | bash

# Mirror for users in mainland China (if direct connection is slow or blocked)
curl -fsSL https://ghproxy.net/https://raw.githubusercontent.com/polarisagi/polaris/main/scripts/install.sh | bash
# Backup mirror
curl -fsSL https://mirror.ghproxy.com/https://raw.githubusercontent.com/polarisagi/polaris/main/scripts/install.sh | bash
```

### Windows

Open PowerShell (Run as Administrator) and paste:

```powershell
# Standard install
irm https://raw.githubusercontent.com/polarisagi/polaris/main/scripts/install.ps1 | iex

# Mirror for users in mainland China
irm https://ghproxy.net/https://raw.githubusercontent.com/polarisagi/polaris/main/scripts/install.ps1 | iex
# Backup mirror
irm https://mirror.ghproxy.com/https://raw.githubusercontent.com/polarisagi/polaris/main/scripts/install.ps1 | iex
```

> 💡 You can also download pre-built packages directly from the [GitHub Releases](https://github.com/polarisagi/polaris/releases) page.

---

## Uninstallation

To stop the background service and remove the Polaris application and desktop shortcuts, run the command below.

> 🔒 **Your data stays safe**: Uninstalling keeps your `~/.polaris` data folder (chat history, memory, and settings) intact so you never lose data by accident.

### macOS / Linux

```bash
# Standard uninstall
curl -fsSL https://raw.githubusercontent.com/polarisagi/polaris/main/scripts/uninstall.sh | bash

# Mirror for mainland China
curl -fsSL https://ghproxy.net/https://raw.githubusercontent.com/polarisagi/polaris/main/scripts/uninstall.sh | bash
```

To also remove all local data and settings permanently, run:
```bash
rm -rf ~/.polaris
```

### Windows (PowerShell)

```powershell
# Standard uninstall
irm https://raw.githubusercontent.com/polarisagi/polaris/main/scripts/uninstall.ps1 | iex

# Mirror for mainland China
irm https://ghproxy.net/https://raw.githubusercontent.com/polarisagi/polaris/main/scripts/uninstall.ps1 | iex
```

To also remove all local data and settings permanently, run in PowerShell:
```powershell
Remove-Item -Path "$env:USERPROFILE\.polaris" -Recurse -Force
```

---

## Build from Source (Optional, for Developers)

Requires Go 1.26+, Rust stable, and Node.js 24+:

```bash
git clone https://github.com/polarisagi/polaris.git
cd polaris
make build
./bin/polaris
```

The compiled executable is placed in `bin/polaris`. Open `http://localhost:28888` in your browser once started.

---

## Community & Contact

Follow the project or reach out for updates, tutorials, and support:

- 🌐 **Official Website**: [https://polarisagi.online/](https://polarisagi.online/)
- 👤 **Author Handle**: **`mrlaoliai`** (same ID across X / Twitter, TikTok, Xiaohongshu, Douyin, etc.)
- 📧 **Email**: [polarisagi.online@gmail.com](mailto:polarisagi.online@gmail.com)
- 📦 **Downloads**: [GitHub Releases](https://github.com/polarisagi/polaris/releases)
- 💬 **Feedback & Bug Reports**: [GitHub Issues](https://github.com/polarisagi/polaris/issues)
- 🛡️ **Security**: Please report security vulnerabilities privately via [GitHub Security Advisories](https://github.com/polarisagi/polaris/security/advisories/new) or email [polarisagi.online@gmail.com](mailto:polarisagi.online@gmail.com) (see [SECURITY.md](SECURITY.md)).

---

## License

Polaris is open-source software licensed under the [GNU Affero General Public License v3.0 (AGPL-3.0-or-later)](LICENSE).
