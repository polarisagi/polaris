#!/bin/bash
set -e

REPO="polarisagi/polaris"
BIN_NAME="polaris"
INSTALL_DIR="$HOME/.polarisagi/polaris/bin"
DATA_DIR="$HOME/.polarisagi/polaris"
PLIST_LABEL="com.polarisagi.polaris"
PLIST_PATH="$HOME/Library/LaunchAgents/${PLIST_LABEL}.plist"
PORT=28888

if [[ "$LANG" == *"zh"* ]] || [[ "$LC_ALL" == *"zh"* ]] || [[ "$LANGUAGE" == *"zh"* ]]; then
    LANG_ZH=true
else
    LANG_ZH=false
fi

msg() {
    if [ "$LANG_ZH" = true ]; then echo "$1"; else echo "$2"; fi
}

msg "🌌 正在安装/更新 PolarisAGI Polaris..." \
    "🌌 Installing/Updating PolarisAGI Polaris..."

OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)

case "$ARCH" in
    x86_64)          ARCH="amd64" ;;
    aarch64|arm64)   ARCH="arm64" ;;
    *)
        msg "❌ 不支持的架构: $ARCH" "❌ Unsupported architecture: $ARCH"
        exit 1
        ;;
esac

# ── 0. 确定网络环境 (Network Detection) ───────────────────────────────────────
PROXY_HOSTS=("https://ghproxy.net" "https://mirror.ghproxy.com")
GH_PROXY=""

# 判断当前网络环境是否处于中国大陆
is_mainland_china() {
    local timeout=2
    local country=""
    
    # 尝试 1: ipinfo.io
    country=$(curl -sSf -m $timeout https://ipinfo.io/country 2>/dev/null | tr -d '[:space:]')
    if [ -n "$country" ]; then
        [ "$country" = "CN" ] && return 0 || return 1
    fi
    
    # 尝试 2: cloudflare trace
    local cf_trace=$(curl -sSf -m $timeout https://1.1.1.1/cdn-cgi/trace 2>/dev/null || true)
    if [ -n "$cf_trace" ] && echo "$cf_trace" | grep -q "loc="; then
        if echo "$cf_trace" | grep -q "loc=CN"; then
            return 0
        else
            return 1
        fi
    fi
    
    # 尝试 3: ip.sb
    local ipsb=$(curl -sSf -m $timeout https://api.ip.sb/geoip 2>/dev/null || true)
    if [ -n "$ipsb" ] && echo "$ipsb" | grep -q 'country_code'; then
        if echo "$ipsb" | grep -q '"country_code":"CN"' || echo "$ipsb" | grep -q '"country_code": "CN"'; then
            return 0
        else
            return 1
        fi
    fi
    
    # 降级：如果所有 IP 接口都失败，测速 Github
    if ! curl -sSf -I --max-time 1 "https://github.com" > /dev/null 2>&1; then
        return 0 # Github 连不上或很慢，假设是大陆
    else
        return 1 # Github 能连上，假设非大陆
    fi
}

msg "🌐 正在检测网络环境归属地及代理情况..." "🌐 Detecting network geolocation and VPN..."

if ! is_mainland_china; then
    msg "✅ 当前网络为海外 IP 或已开启全局代理，将使用直连。" \
        "✅ Network is outside mainland China or VPN active. Using direct connection."
else
    msg "⚠️  检测到当前网络位于中国大陆且未全局代理，寻找镜像代理..." \
        "⚠️  Mainland China network detected without VPN. Switching to proxy mirrors..."
    for p in "${PROXY_HOSTS[@]}"; do
        if curl -sSf -I --max-time 5 "$p" > /dev/null 2>&1; then
            GH_PROXY="${p}/"
            msg "   ✅ 选用镜像代理: $p" "   ✅ Using proxy: $p"
            break
        fi
    done
fi


# ── 1. 环境依赖预检与自动安装 (Dependencies) ──────────────────────────────────
msg "🔍 正在检查并自动安装必要运行环境 (Checking & installing dependencies)..." \
    "🔍 Checking and installing required dependencies..."

# 1. Git (唯一依赖系统全局安装的工具)
if ! command -v git >/dev/null 2>&1; then
    if [ "$OS" = "darwin" ]; then
        msg "⚠️  macOS 缺少 Git。正在唤起系统安装向导..." \
            "⚠️  macOS is missing Git. Triggering system install wizard..."
        xcode-select --install || true
        msg "💡 请在弹窗中完成安装后，重新运行本脚本。" \
            "💡 Please complete the installation in the prompt and re-run this script."
        exit 1
    else
        msg "⚠️  未检测到 Git。请先通过包管理器 (如 apt/yum) 安装 git 后重试。" \
            "⚠️  Git not found. Please install it via your package manager (apt/yum) and retry."
        exit 1
    fi
fi

# 2. uv (跨平台包管理器)
if ! command -v uv >/dev/null 2>&1; then
    msg "   📦 正在安装 uv..." "   📦 Installing uv..."
    curl -LsSf https://astral.sh/uv/install.sh | sh
    export PATH="$HOME/.cargo/bin:$HOME/.local/bin:$PATH"
fi

# 3. Python (通过 uv 独立安装)
if ! command -v python3 >/dev/null 2>&1 && ! command -v python >/dev/null 2>&1; then
    msg "   🐍 正在通过 uv 安装 Python 环境..." "   🐍 Installing Python via uv..."
    uv python install 3
    
    # 将 uv 安装的 python 软链接到 PATH 中，确保后续服务能找到
    UV_PYTHON_BIN=$(uv python find 3 2>/dev/null || true)
    if [ -n "$UV_PYTHON_BIN" ]; then
        mkdir -p "$HOME/.local/bin"
        ln -sf "$UV_PYTHON_BIN" "$HOME/.local/bin/python3"
        ln -sf "$UV_PYTHON_BIN" "$HOME/.local/bin/python"
    fi
fi

# 4. Node.js (通过 nvm 安装)
if ! command -v node >/dev/null 2>&1; then
    msg "   🟢 正在安装 Node.js (通过 nvm)..." "   🟢 Installing Node.js (via nvm)..."
    export NVM_DIR="$HOME/.nvm"
    if [ -n "$GH_PROXY" ]; then
        curl -o- "${GH_PROXY}https://raw.githubusercontent.com/nvm-sh/nvm/v0.40.5/install.sh" | NVM_SOURCE="${GH_PROXY}https://github.com/nvm-sh/nvm.git" bash
        # 为 nvm 设置国内节点镜像，防止 node 下载超时
        export NVM_NODEJS_ORG_MIRROR="https://npmmirror.com/mirrors/node/"
    else
        curl -o- https://raw.githubusercontent.com/nvm-sh/nvm/v0.40.5/install.sh | bash
    fi
    [ -s "$NVM_DIR/nvm.sh" ] && \. "$NVM_DIR/nvm.sh"
    nvm install 24
fi

msg "✅ 基础环境准备就绪 (Dependencies ready)。" "✅ Dependencies ready."

# ── 2. 确定 Polaris 下载源 ─────────────────────────────────────────────────
ARCHIVE_NAME="${BIN_NAME}-${OS}-${ARCH}.tar.gz"
GITHUB_BASE="https://github.com/${REPO}/releases/latest/download"
DIRECT_URL="${GITHUB_BASE}/${ARCHIVE_NAME}"

CANDIDATE_URLS=()
if [ -z "$GH_PROXY" ]; then
    CANDIDATE_URLS+=("$DIRECT_URL")
    for p in "${PROXY_HOSTS[@]}"; do
        CANDIDATE_URLS+=("${p}/${DIRECT_URL}")
    done
else
    CANDIDATE_URLS+=("${GH_PROXY}${DIRECT_URL}")
    for p in "${PROXY_HOSTS[@]}"; do
        if [ "${p}/" != "$GH_PROXY" ]; then
            CANDIDATE_URLS+=("${p}/${DIRECT_URL}")
        fi
    done
    CANDIDATE_URLS+=("$DIRECT_URL")
fi

if [ ${#CANDIDATE_URLS[@]} -eq 0 ]; then
    msg "❌ 无可用下载源。" "❌ No download sources available."
    exit 1
fi

# ── 2. 下载（支持断点续传，逐源重试）──────────────────────────────────────────
TMP_ARCHIVE="/tmp/${BIN_NAME}-install.tar.gz"
TMP_DIR="/tmp/${BIN_NAME}-install-$$"

msg "⬇️  开始下载（支持断点续传）..." "⬇️  Downloading (with resume support)..."

DOWNLOADED=false
for URL in "${CANDIDATE_URLS[@]}"; do
    msg "   尝试: $URL" "   Trying: $URL"
    # -C - 启用断点续传（curl 自动读取 TMP_ARCHIVE 已有字节数追加）
    # -f 非 2xx 状态码报错; --max-time 300 单次下载最多等 5min
    if curl -C - -sSLf --progress-bar --max-time 300 -o "$TMP_ARCHIVE" "$URL"; then
        DOWNLOADED=true
        break
    else
        EXIT_CODE=$?
        # curl exit 33 = 服务端不支持 Range，尝试重新全量下载
        if [ "$EXIT_CODE" -eq 33 ]; then
            rm -f "$TMP_ARCHIVE"
            if curl -sSLf --progress-bar --max-time 300 -o "$TMP_ARCHIVE" "$URL"; then
                DOWNLOADED=true
                break
            fi
        fi
        msg "   此源失败（exit $EXIT_CODE），尝试下一个..." \
            "   Source failed (exit $EXIT_CODE), trying next..."
    fi
done

if [ "$DOWNLOADED" = false ]; then
    msg "❌ 所有下载源均失败，请检查网络连接或稍后重试。" \
        "❌ All download sources failed. Check your network or retry later."
    rm -f "$TMP_ARCHIVE"
    exit 1
fi

# ── 3. 校验 & 解压 ────────────────────────────────────────────────────────────
msg "📦 正在校验并解压..." "📦 Verifying and extracting..."

# 验证归档完整性（不依赖 sha256sum 命令，跨平台兼容性更好）
if ! tar -tzf "$TMP_ARCHIVE" > /dev/null 2>&1; then
    msg "❌ 归档文件损坏，已删除，请重新运行安装脚本。" \
        "❌ Archive corrupted, deleted. Re-run the install script."
    rm -f "$TMP_ARCHIVE"
    exit 1
fi

mkdir -p "$TMP_DIR"
tar -xzf "$TMP_ARCHIVE" -C "$TMP_DIR"
rm -f "$TMP_ARCHIVE"

# ── 4. 停止旧服务 ─────────────────────────────────────────────────────────────
if [ "$OS" = "darwin" ]; then
    if launchctl list 2>/dev/null | grep -q "$PLIST_LABEL"; then
        msg "🛑 正在停止旧 macOS 服务..." "🛑 Stopping existing macOS service..."
        launchctl unload "$PLIST_PATH" 2>/dev/null || true
    fi
    pkill -f "${INSTALL_DIR}/${BIN_NAME}" 2>/dev/null || true

elif [ "$OS" = "linux" ] && command -v systemctl >/dev/null 2>&1; then
    SYSTEMD_USER_DIR="$HOME/.config/systemd/user"
    if systemctl --user is-active --quiet "$BIN_NAME" 2>/dev/null; then
        msg "🛑 正在停止旧 Linux 用户服务..." "🛑 Stopping existing Linux user service..."
        systemctl --user stop "$BIN_NAME" || true
    fi
fi

# ── 5. 安装文件及依赖资源 ─────────────────────────────────────────────────────
mkdir -p "$INSTALL_DIR"
cp -R "$TMP_DIR"/* "$INSTALL_DIR/" 2>/dev/null || cp -r "$TMP_DIR"/* "$INSTALL_DIR/"
chmod +x "${INSTALL_DIR}/${BIN_NAME}"
rm -rf "$TMP_DIR"

msg "✅ 程序已安装: ${INSTALL_DIR}/${BIN_NAME}" \
    "✅ Binary installed: ${INSTALL_DIR}/${BIN_NAME}"

# ── 6. 配置系统服务 ───────────────────────────────────────────────────────────
# 提前准备服务所需要的 PATH 环境变量
NVM_BIN_PATH=""
if [ -s "$HOME/.nvm/nvm.sh" ]; then
    \. "$HOME/.nvm/nvm.sh" >/dev/null 2>&1
    NVM_BIN_PATH=$(dirname "$(nvm which current 2>/dev/null)" 2>/dev/null || echo "")
fi

SERVICE_PATH="/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin:${HOME}/.cargo/bin:${HOME}/.local/bin:${INSTALL_DIR}"
if [ -n "$NVM_BIN_PATH" ]; then
    SERVICE_PATH="${NVM_BIN_PATH}:${SERVICE_PATH}"
fi

if [ "$OS" = "darwin" ]; then
    msg "⚙️  配置 macOS launchd 后台服务..." "⚙️  Configuring macOS launchd service..."
    mkdir -p "$HOME/Library/LaunchAgents"
    mkdir -p "$DATA_DIR/logs"

    cat > "$PLIST_PATH" <<PLIST_EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>${PLIST_LABEL}</string>
    <key>ProgramArguments</key>
    <array>
        <string>${INSTALL_DIR}/${BIN_NAME}</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>WorkingDirectory</key>
    <string>${HOME}</string>
    <key>StandardOutPath</key>
    <string>${DATA_DIR}/logs/polaris.log</string>
    <key>StandardErrorPath</key>
    <string>${DATA_DIR}/logs/polaris.log</string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>HOME</key>
        <string>${HOME}</string>
        <key>PATH</key>
        <string>${SERVICE_PATH}</string>
    </dict>
</dict>
</plist>
PLIST_EOF

    launchctl load "$PLIST_PATH"
    msg "✅ macOS 服务已配置，随登录自动启动。" \
        "✅ macOS service configured, auto-starts on login."

elif [ "$OS" = "linux" ] && command -v systemctl >/dev/null 2>&1; then
    msg "⚙️  配置 Linux 用户级 systemd 服务..." "⚙️  Configuring Linux user systemd service..."
    SYSTEMD_USER_DIR="$HOME/.config/systemd/user"
    mkdir -p "$SYSTEMD_USER_DIR"
    mkdir -p "$DATA_DIR/logs"

    cat > "$SYSTEMD_USER_DIR/${BIN_NAME}.service" <<UNIT_EOF
[Unit]
Description=PolarisAGI Polaris AI Agent
After=network.target

[Service]
Environment="PATH=${SERVICE_PATH}"
ExecStart=${INSTALL_DIR}/${BIN_NAME}
Restart=on-failure
RestartSec=5
WorkingDirectory=${HOME}
StandardOutput=append:${DATA_DIR}/logs/polaris.log
StandardError=append:${DATA_DIR}/logs/polaris.log

[Install]
WantedBy=default.target
UNIT_EOF

    loginctl enable-linger "$USER" 2>/dev/null || true
    systemctl --user daemon-reload
    systemctl --user enable "$BIN_NAME"
    systemctl --user restart "$BIN_NAME"
    msg "✅ systemd 用户服务已启动。查看状态: systemctl --user status polaris" \
        "✅ systemd user service started. Check: systemctl --user status polaris"
fi

# ── 7. PATH 提示 ──────────────────────────────────────────────────────────────
echo ""
msg "🎉 安装完成！请访问 http://127.0.0.1:${PORT} 打开控制台。" \
    "🎉 Installation complete! Visit http://127.0.0.1:${PORT} to open the console."
msg "💡 若需命令行直接使用 polaris，请将以下路径加入 PATH：" \
    "💡 To use polaris in CLI, add to PATH:"
echo "   export PATH=\"\$PATH:${INSTALL_DIR}\""
