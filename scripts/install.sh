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

# ── 1. 确定下载候选 URL 列表 ─────────────────────────────────────────────────
ARCHIVE_NAME="${BIN_NAME}-${OS}-${ARCH}.tar.gz"
GITHUB_BASE="https://github.com/${REPO}/releases/latest/download"
DIRECT_URL="${GITHUB_BASE}/${ARCHIVE_NAME}"
PROXY_HOSTS=("https://ghproxy.net" "https://mirror.ghproxy.com")

# 与 Go 端 autoProbe 保持一致：500ms 内可达 github.com → 判定海外/VPN 直连
# 海外到 GitHub 通常 <200ms；中国大陆通常 ≥1s 或超时
msg "🌐 正在检测网络环境（500ms 阈值）..." "🌐 Detecting network environment (500ms threshold)..."

CANDIDATE_URLS=()
if curl -sSf -I --max-time 0.5 "https://github.com" > /dev/null 2>&1; then
    # 低延迟直连：海外或 VPN，优先直连
    msg "✅ GitHub 低延迟直连（<500ms），无需代理。" \
        "✅ GitHub low-latency direct (<500ms), no proxy needed."
    CANDIDATE_URLS+=("$DIRECT_URL")
    for p in "${PROXY_HOSTS[@]}"; do
        CANDIDATE_URLS+=("${p}/${DIRECT_URL}")
    done
else
    # 高延迟或超时：判定为中国大陆受限网络，优先使用镜像代理
    msg "⚠️  GitHub 响应慢（>500ms），判定为中国大陆网络，切换镜像代理..." \
        "⚠️  GitHub slow (>500ms), likely China mainland network, switching to proxy mirrors..."
    for p in "${PROXY_HOSTS[@]}"; do
        if curl -sSf -I --max-time 5 "$p" > /dev/null 2>&1; then
            CANDIDATE_URLS+=("${p}/${DIRECT_URL}")
            msg "   ✅ 可用镜像: $p" "   ✅ Reachable proxy: $p"
        else
            msg "   ⚠️  不可用: $p" "   ⚠️  Unreachable: $p"
        fi
    done
    # 直连作为最后备用（中国大陆不稳定但有时可用）
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
        <string>/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin:${INSTALL_DIR}</string>
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
