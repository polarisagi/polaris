# Polaris

> **真正属于你自己的私人 AI 助理** —— 安装在你的电脑或服务器上，数据 100% 留在本地，帮你管资料、写代码、跑自动化、处理日常繁琐工作。

[![License: AGPL v3](https://img.shields.io/badge/License-AGPL_v3-blue.svg)](LICENSE)
[![Website](https://img.shields.io/badge/Website-polarisagi.online-00ADD8)](https://polarisagi.online/)

[English](README.md) | **中文**

---

## Polaris 是什么？

今天大多数 AI 工具要么运行在别人的云端服务器上（你的对话、文档和隐私数据都留在第三方平台），要么只是一个“聊天窗口”——能陪你说话，却没法真正替你把事情做完。

**Polaris 是一个开源、可自托管（Self-Hosted）的全能个人 AI 助理。**

你可以用一行命令把它安装在自己的 Mac、Windows 电脑、Linux 主机或云服务器上。安装后，直接用日常语言吩咐它做事：整理本地文件、搜集并精读资料、编写与调试代码、定时汇总信息、自动化重复性办公任务。**你所有的对话记录、个人知识库和文件数据都保存在你自己的设备里，绝不经过任何第三方中转服务器。**

---

## 为什么选择 Polaris？

### 1. 真正能把复杂事情做完，而不只是陪聊

很多 AI 助手处理超过三步的任务就会开始“胡言乱语”、中途卡死或跑偏。Polaris 内置了稳健的任务执行引擎：面对复杂目标时，它会自动拆解步骤、逐步验证结果，哪怕中间某一步出错也能自动回退重试，稳扎稳打地把任务交付给你。

### 2. 简单问题秒回，复杂难题深思

不是每句话都需要调用最昂贵、最慢的模型。Polaris 会自动判断问题的难易程度：
- **日常熟手任务**：直接调用已掌握的本地技能，毫秒级响应，不浪费一分钱算力；
- **中等难度问题**：快速推理，即刻作答；
- **从未见过的复杂难题**：自动开启深度思考与多路径推演，确保给出高质量方案。

### 3. 三位一体的专属 AI 团队

在 Polaris 内部，不是一个 AI 在单打独斗，而是三个专职角色在后台协同为你服务：
- **主助理**：全程与你对话，负责理解意图、统筹并推进任务；
- **资料员**：在后台静默翻阅你的本地文档、知识库与历史记录，随时为主助理补充背景信息；
- **安全官**：独立于业务之外，在每一项操作执行前审查安全性，拦截任何潜在风险。

### 4. 四层长期记忆，越用越懂你

绝大多数 AI 关掉窗口就忘了你是谁。Polaris 模仿人类的记忆方式，构建了四层记忆体系：
- 记住**当前正在做的事**（工作记忆）；
- 记住**过去发生过的每一次对话与事件**（历史记忆，支持跨会话精准回溯）；
- 记住**你的个人偏好、专业领域与概念关联**（知识图谱记忆）；
- 记住**已经跑通的成功工作流**（技能记忆，下次遇到同类任务直接复用）。

你用得越久，它就越了解你的工作习惯与喜好。

### 5. 隐私与安全从底层锁死

- **数据完全自有**：所有数据存储在本地 `~/.polaris/` 目录，完全由你掌控；
- **隔离沙箱执行**：运行脚本或工具时在安全沙箱内隔离执行，不会破坏你的电脑系统；
- **防网页投毒**：自动将外部网页、第三方返回的数据与你的核心指令严格隔离，防止被恶意网页内容诱导执行危险操作；
- **权限分级可控**：支持“只读模式”、“敏感操作需你确认”和“全自动模式”，涉及重要文件或外部操作时由你亲自把关。

### 6. 从经验中持续成长，不重复踩坑

Polaris 会自动总结每一次任务中的成功经验与失败教训。遇到曾经踩过的坑，它会提前预警并自动避开；所有自我优化都会先经过验证测试，确保真正变好才会保留，绝不会“越用越笨”。

---

## 核心功能一览

- 🖥️ **桌面客户端 + 浏览器双形态**：自带轻量原生桌面应用（支持 macOS / Windows / Linux，系统托盘常驻、开机自启、系统通知），同时也支持直接在浏览器中打开使用。
- 🎙️ **自然语音对话**：支持本地离线语音输入（语音转文字）与自然语音朗读，语音模型按需自动下载、空闲时自动释放内存。
- 💬 **连接你常用的聊天软件（18+ 渠道）**：支持接入飞书、钉钉、企业微信、Telegram、Discord、Slack、QQ 机器人、邮件、Webhook、Home Assistant 等。出门在外也能用手机随时给家里的 Polaris 下达指令。
- ⏰ **定时任务与自动化**：用自然语言设定定时任务（例如每日资讯汇总、定时整理归档、周期性检查），后台静默执行并向你汇报结果。
- 🧩 **官方插件市场与无限扩展**：支持一键安装 [官方开源插件市场](https://github.com/polarisagi/polaris-plugins-official) 中的各类技能与 MCP 工具；也可将自定义脚本放入 `~/.polaris/hooks/` 自由扩展。
- 🤝 **多台电脑协同工作**：如果你同时拥有笔记本、台式机或云服务器，可以让多台设备上的 Polaris 安全配对、互相委派任务，还能与其他支持 A2A 协议的智能体互联互通。
- 🔌 **自由选择大模型（支持本地离线模型）**：开箱默认推荐高性价比的 DeepSeek，同时无缝支持 OpenAI、Anthropic (Claude)、Google (Gemini) 等主流服务商，以及通过 Ollama 运行完全离线的本地大模型；对外还提供标准 OpenAI 兼容接口。

---

## 系统与硬件要求

Polaris 极其轻量，无需安装任何复杂的外部数据库或组件，单文件开箱即用：

- **支持系统**：macOS（Apple Silicon / Intel）、Windows（64 位 / ARM64）、Linux（x86_64 / ARM64）
- **最低配置**：2 GB 内存（普通入门级云服务器 / VPS 即可运行核心功能）
- **推荐配置**：8 GB 及以上内存（日常个人电脑、笔记本、Mac mini 均可流畅体验全部功能；16 GB+ 内存可流畅运行本地离线小模型）

---

## 一键安装

打开电脑终端，复制粘贴以下一行命令即可自动完成下载、安装并配置开机自启。安装完成后，Polaris 会自动启动。

### macOS / Linux

在终端（Terminal）中执行：

```bash
# 标准安装
curl -fsSL https://raw.githubusercontent.com/polarisagi/polaris/main/scripts/install.sh | bash

# 中国大陆网络加速（如果上方直连较慢或失败，推荐使用）
curl -fsSL https://ghproxy.net/https://raw.githubusercontent.com/polarisagi/polaris/main/scripts/install.sh | bash
# 备用加速源
curl -fsSL https://mirror.ghproxy.com/https://raw.githubusercontent.com/polarisagi/polaris/main/scripts/install.sh | bash
```

### Windows

以管理员身份打开 PowerShell，执行：

```powershell
# 标准安装
irm https://raw.githubusercontent.com/polarisagi/polaris/main/scripts/install.ps1 | iex

# 中国大陆网络加速（如果上方直连较慢或失败，推荐使用）
irm https://ghproxy.net/https://raw.githubusercontent.com/polarisagi/polaris/main/scripts/install.ps1 | iex
# 备用加速源
irm https://mirror.ghproxy.com/https://raw.githubusercontent.com/polarisagi/polaris/main/scripts/install.ps1 | iex
```

> 💡 你也可以前往 [GitHub Releases](https://github.com/polarisagi/polaris/releases) 页面，手动下载对应操作系统的压缩包。

---

## 一键卸载

如果你需要卸载 Polaris，执行以下命令即可停止后台服务、移除程序与桌面快捷方式。

> 🔒 **安全说明**：卸载脚本默认会**保留**你在 `~/.polaris` 目录下的个人数据（对话记录、记忆库、配置文件），防止误删。

### macOS / Linux

```bash
# 标准卸载
curl -fsSL https://raw.githubusercontent.com/polarisagi/polaris/main/scripts/uninstall.sh | bash

# 中国大陆网络加速
curl -fsSL https://ghproxy.net/https://raw.githubusercontent.com/polarisagi/polaris/main/scripts/uninstall.sh | bash
```

如需连同所有本地历史数据一起彻底清除，卸载后可手动执行：
```bash
rm -rf ~/.polaris
```

### Windows（PowerShell）

```powershell
# 标准卸载
irm https://raw.githubusercontent.com/polarisagi/polaris/main/scripts/uninstall.ps1 | iex

# 中国大陆网络加速
irm https://ghproxy.net/https://raw.githubusercontent.com/polarisagi/polaris/main/scripts/uninstall.ps1 | iex
```

如需连同所有本地历史数据一起彻底清除，卸载后可在 PowerShell 中手动执行：
```powershell
Remove-Item -Path "$env:USERPROFILE\.polaris" -Recurse -Force
```

---

## 从源码构建（开发者可选）

如果你想自己从源码编译运行（需预先安装 Go 1.26+、Rust stable、Node.js 24+）：

```bash
git clone https://github.com/polarisagi/polaris.git
cd polaris
make build
./bin/polaris
```

编译产物位于 `bin/polaris`，启动后在浏览器访问 `http://localhost:28888` 即可。

---

## 联系我们与关注作者

欢迎访问官方网站或关注作者账号，获取最新动态、使用教程与交流支持：

- 🌐 **官方网站**：[https://polarisagi.online/](https://polarisagi.online/)
- 👤 **作者全网同名 ID**：**`mrlaoliai`**（小红书、抖音、TikTok、X / Twitter 等平台均同名，欢迎搜索关注）
- 📧 **联系邮箱**：[polarisagi.online@gmail.com](mailto:polarisagi.online@gmail.com)
- 📦 **版本下载**：[GitHub Releases](https://github.com/polarisagi/polaris/releases)
- 💬 **问题反馈**：[GitHub Issues](https://github.com/polarisagi/polaris/issues)
- 🛡️ **安全上报**：如发现安全隐患，请通过 [GitHub Security Advisories](https://github.com/polarisagi/polaris/security/advisories/new) 或发送邮件至 [polarisagi.online@gmail.com](mailto:polarisagi.online@gmail.com) 私下联系（详见 [SECURITY.md](SECURITY.md)）。

---

## 开源许可

Polaris 采用 [GNU Affero General Public License v3.0 (AGPL-3.0-or-later)](LICENSE) 开源许可协议免费开放源码。
