# Polaris

> 一个运行在你自己的电脑上的 AI 助手 · An AI assistant that runs on your own computer.

---

## Polaris 是什么？

Polaris 是一个可以自己托管（self-host）的 AI 助手。把它安装在你自己的电脑或服务器上，用日常语言告诉它要做什么——整理资料、写代码、查资料、处理文档、自动化重复工作。所有数据都留在你自己的设备上，不经过任何第三方服务器。

## 它有什么特点？

- 🔒 **数据自有**：跑在本地，你的对话和数据不会上传给第三方。
- 🧩 **可扩展**：通过插件和 MCP 工具接入更多能力。
- 🖥️ **桌面 + 浏览器**：既可以用桌面应用，也可以在浏览器里使用。
- 🆓 **开源免费**：采用 AGPL-3.0 许可，源码公开。

## 如何安装？

### macOS / Linux

在终端里粘贴这一行：

```sh
curl -fsSL https://raw.githubusercontent.com/polarisagi/polaris-agent/main/scripts/install.sh | bash
```

### Windows

在 PowerShell 里粘贴这一行：

```powershell
irm https://raw.githubusercontent.com/polarisagi/polaris-agent/main/scripts/install.ps1 | iex
```

安装完成后，Polaris 会自动启动，并告诉你该在浏览器里打开哪个地址。

> 也可以到 [Releases](https://github.com/polarisagi/polaris-agent/releases) 页面直接下载对应平台的安装包。

## 从源码构建（给开发者）

需要 Go 1.26+、Rust stable、Node.js 24+。

```sh
make build
```

生成的可执行文件在 `bin/polaris`。

## 帮助与反馈

- [Releases](https://github.com/polarisagi/polaris-agent/releases)：下载安装包
- [Issues](https://github.com/polarisagi/polaris-agent/issues)：反馈问题

## 许可

Polaris 采用 [AGPL-3.0-or-later](LICENSE) 许可。

## 安全

发现安全漏洞请通过 [GitHub Security Advisories](https://github.com/polarisagi/polaris-agent/security/advisories/new) 私下报告，详见 [SECURITY.md](SECURITY.md)。

---

# Polaris

> An AI assistant that runs on your own computer. · 一个运行在你自己的电脑上的 AI 助手。

## What is Polaris?

Polaris is a self-hosted AI assistant. Install it on your own computer or server, then tell it what to do in plain language — organize information, write code, research, handle documents, automate repetitive work. All your data stays on your own device and never passes through a third-party server.

## Highlights

- 🔒 **Your data stays yours**: runs locally; nothing is uploaded to a third party.
- 🧩 **Extensible**: add plugins and MCP tools.
- 🖥️ **Desktop & browser**: use a desktop app or the browser UI.
- 🆓 **Open source**: AGPL-3.0, source available.

## Install

### macOS / Linux

Paste this into a terminal:

```sh
curl -fsSL https://raw.githubusercontent.com/polarisagi/polaris-agent/main/scripts/install.sh | bash
```

### Windows

Paste this into PowerShell:

```powershell
irm https://raw.githubusercontent.com/polarisagi/polaris-agent/main/scripts/install.ps1 | iex
```

When installation finishes, Polaris starts and tells you which address to open in your browser.

> You can also download a package from [Releases](https://github.com/polarisagi/polaris-agent/releases).

## Build from source (for developers)

Requires Go 1.26+, Rust stable, and Node.js 24+.

```sh
make build
```

The executable is written to `bin/polaris`.

## Help

- [Releases](https://github.com/polarisagi/polaris-agent/releases): download packages
- [Issues](https://github.com/polarisagi/polaris-agent/issues): report a problem

## License

Polaris is licensed under [AGPL-3.0-or-later](LICENSE).

## Security

Report vulnerabilities privately via [GitHub Security Advisories](https://github.com/polarisagi/polaris-agent/security/advisories/new). See [SECURITY.md](SECURITY.md).
