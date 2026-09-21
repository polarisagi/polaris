#!/usr/bin/env bash
# -e: 任何命令失败立即退出；-u: 引用未定义变量报错；-o pipefail: 管道任一步骤失败即报错
set -euo pipefail

REPO="polarisagi/polaris"
BIN_NAME="polaris"
INSTALL_DIR="$HOME/.polarisagi/polaris/bin"
DATA_DIR="$HOME/.polarisagi/polaris"
PLIST_LABEL="com.polarisagi.polaris"
PLIST_PATH="$HOME/Library/LaunchAgents/${PLIST_LABEL}.plist"
PORT=28888

# 版本常量——升级时只改这两处，与 ci_test.sh GOLANGCI_LINT_VERSION 管理模式保持一致
NVM_VERSION="v0.40.5"  # https://github.com/nvm-sh/nvm/releases
NODE_VERSION="24"

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
        curl -o- "${GH_PROXY}https://raw.githubusercontent.com/nvm-sh/nvm/${NVM_VERSION}/install.sh" | NVM_SOURCE="${GH_PROXY}https://github.com/nvm-sh/nvm.git" bash
        # 为 nvm 设置国内节点镜像，防止 node 下载超时
        export NVM_NODEJS_ORG_MIRROR="https://npmmirror.com/mirrors/node/"
    else
        curl -o- "https://raw.githubusercontent.com/nvm-sh/nvm/${NVM_VERSION}/install.sh" | bash
    fi
    [ -s "$NVM_DIR/nvm.sh" ] && \. "$NVM_DIR/nvm.sh"
    nvm install "$NODE_VERSION"
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

# 3a. SHA-256 完整性校验（GR-7.4）：release.yml package job 为每个归档同步
# 生成并发布 <archive>.sha256（sha256sum 标准格式 "<hash>  <filename>"），
# 从与归档成功下载的同一来源（$URL，保留 for 循环退出时的最后取值）下载
# 对应的 .sha256 文件比对，防止下载源被中间人替换/投毒后仅凭 tar 结构校验
# 无法识破（只要是合法 tar.gz 格式即可通过 tar -tzf）。
#
# 沿用既有约束"不依赖 sha256sum 命令，跨平台兼容性更好"：改用 openssl dgst
# -sha256（macOS/Linux 均预装）计算本地哈希，而不是引入平台相关的 sha256sum。
TMP_SHA_FILE="${TMP_ARCHIVE}.sha256"
rm -f "$TMP_SHA_FILE"

# 校验和默认只从官方直连源下载，失败即中止（防止镜像被投毒后连"标尺"本身
# 都被污染——如果哈希也来自镜像，篡改过二进制的镜像可以同时伪造一份匹配的
# 校验和，比对形同虚设）。
#
# 但对完全无法直连 GitHub 的网络环境（常见于大陆网络，非"仅仅慢"而是彻底
# 不可达），一律中止会让二进制虽已通过镜像下载成功、却因这一步无法完成而
# 前功尽弃。系统最优设计：默认保持安全（不做任何静默/自动降级），仅在用户
# 显式设置 POLARIS_ALLOW_MIRROR_CHECKSUM=1 时才允许回退到与二进制相同的镜像源
# 获取校验和，并在回退前打印明确的风险警告（该模式下无法防御"镜像同时伪造
# 二进制与校验和"的攻击，用户需自行承担该风险，例如已通过其他渠道信任该镜像，
# 或后续自行核对官方发布页面公布的哈希）。
if ! curl -sSLf --max-time 30 -o "$TMP_SHA_FILE" "${DIRECT_URL}.sha256"; then
    # 回退源固定用 $URL（下载二进制那个 for 循环实际成功的源，循环 break 后
    # 该变量仍保留其值）而非重新猜测/拼接镜像地址：语义上是"信任刚才已经把
    # 二进制发给我的同一个源"，而不是引入另一个与二进制来源无关的新镜像。
    # 若二进制本身就是从官方直连源下载的（$URL == $DIRECT_URL），这里再次
    # 尝试同一 URL 没有意义，直接跳过回退。
    if [ "$POLARIS_ALLOW_MIRROR_CHECKSUM" = "1" ] && [ "$URL" != "$DIRECT_URL" ]; then
        msg "⚠️  无法从官方直连源获取校验和文件。检测到 POLARIS_ALLOW_MIRROR_CHECKSUM=1，" \
            "⚠️  Failed to fetch checksum from the official source. POLARIS_ALLOW_MIRROR_CHECKSUM=1 detected,"
        msg "⚠️  按你的显式授权回退到二进制同源（$URL）下载校验和。此模式下无法防御该源同时" \
            "⚠️  falling back to the same source that served the binary ($URL) for the checksum. This mode cannot detect"
        msg "⚠️  伪造二进制与校验和的供应链攻击，风险自负。" \
            "⚠️  a source that forges both the binary and its checksum together — proceed at your own risk."
        if ! curl -sSLf --max-time 30 -o "$TMP_SHA_FILE" "${URL}.sha256"; then
            msg "❌ 该源同样无法获取校验和文件，已中止安装。" \
                "❌ Checksum also unreachable via that source. Aborting install."
            rm -f "$TMP_ARCHIVE" "$TMP_SHA_FILE"
            exit 1
        fi
    else
        msg "❌ 无法从官方直连源获取校验和文件，出于供应链安全考虑（默认严禁使用镜像源）已中止安装。" \
            "❌ Failed to download checksum file from official source. Aborting install for supply-chain safety (mirrors are forbidden for checksums by default)."
        msg "   若你所在网络无法直连 GitHub，可显式设置 POLARIS_ALLOW_MIRROR_CHECKSUM=1 后重新运行本脚本以" \
            "   If your network cannot reach GitHub directly, you may explicitly set POLARIS_ALLOW_MIRROR_CHECKSUM=1 and re-run this script to"
        msg "   回退到镜像源获取校验和（会降低供应链攻击防御能力，请仅在信任该镜像时使用）。" \
            "   fall back to a mirror for the checksum (this weakens supply-chain attack defenses — only use it if you trust the mirror)."
        rm -f "$TMP_ARCHIVE" "$TMP_SHA_FILE"
        exit 1
    fi
fi

if ! command -v openssl >/dev/null 2>&1; then
    msg "❌ 未找到 openssl，无法校验下载包完整性，已中止安装。请安装 openssl 后重试。" \
        "❌ openssl not found; cannot verify package integrity. Aborting install. Please install openssl and retry."
    rm -f "$TMP_ARCHIVE" "$TMP_SHA_FILE"
    exit 1
fi

EXPECTED_SHA=$(awk '{print $1}' "$TMP_SHA_FILE")
ACTUAL_SHA=$(openssl dgst -sha256 "$TMP_ARCHIVE" | awk '{print $NF}')
rm -f "$TMP_SHA_FILE"

if [ -z "$EXPECTED_SHA" ] || [ "$EXPECTED_SHA" != "$ACTUAL_SHA" ]; then
    msg "❌ 下载的安装包校验和不匹配，可能遭遇中间人篡改，安装已中止。" \
        "❌ Downloaded package checksum mismatch; possible tampering detected. Installation aborted."
    rm -f "$TMP_ARCHIVE"
    exit 1
fi
msg "✅ 校验和匹配，归档完整性已确认。" "✅ Checksum verified, archive integrity confirmed."

# 3b. 归档结构校验（哈希校验通过后的第二道检查，双重保险）
if ! tar -tzf "$TMP_ARCHIVE" > /dev/null 2>&1; then
    msg "❌ 归档文件损坏，已删除，请重新运行安装脚本。" \
        "❌ Archive corrupted, deleted. Re-run the install script."
    rm -f "$TMP_ARCHIVE"
    exit 1
fi

mkdir -p "$TMP_DIR"
tar -xzf "$TMP_ARCHIVE" -C "$TMP_DIR" --strip-components=1
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

# ── 6. macOS ad-hoc 签名 ──────────────────────────────────────────────────────
# Apple Silicon 拒绝加载无签名的可执行文件与 dylib。ad-hoc 签名（codesign -s -）
# 免费、不需要开发者账号，且本脚本走 curl 下载——不会写 com.apple.quarantine
# 属性，故 Gatekeeper 不介入（ADR-0096 决策八）。
if [ "$OS" = "darwin" ] && command -v codesign >/dev/null 2>&1; then
    codesign --force --sign - "${INSTALL_DIR}/${BIN_NAME}" 2>/dev/null || true
    for lib in "${INSTALL_DIR}"/lib/*.dylib; do
        [ -e "$lib" ] && codesign --force --sign - "$lib" 2>/dev/null || true
    done
    # 浏览器下载过的副本可能带隔离属性，顺手清掉（本脚本自己下的没有）
    xattr -dr com.apple.quarantine "${INSTALL_DIR}" 2>/dev/null || true
fi

# ── 6.5 配置系统服务 ──────────────────────────────────────────────────────────
# 委托给 `polaris service install`，本脚本**不再自写** plist / systemd 单元。
# 两份实现必然漂移其一：标签、ExecStart 参数、日志路径各写一遍，改了一处忘了
# 另一处的结果是"装出来的服务和 polaris service status 看到的不是同一个"。
# 单一实现见 cmd/polaris/cli_service.go（ADR-0096 决策一）。
msg "⚙️  注册系统服务..." "⚙️  Registering system service..."
if "${INSTALL_DIR}/${BIN_NAME}" service install; then
    msg "✅ 服务已注册并启动，随登录自动运行。" \
        "✅ Service registered and started, auto-starts on login."
else
    msg "⚠️  服务注册失败，可稍后手动执行：${INSTALL_DIR}/${BIN_NAME} service install" \
        "⚠️  Service registration failed. Run manually later: ${INSTALL_DIR}/${BIN_NAME} service install"
fi

# ── 6.8 桌面外壳（可选）──────────────────────────────────────────────────────
# 外壳与守护进程**分开安装**（ADR-0096 决策四）：守护进程装在 bin/ 下由自身的
# updater 就地更新，外壳装在系统常规位置。这样就地替换二进制永远发生在 app
# bundle 之外，不触碰任何签名结构。
#
# 默认跳过：无图形界面的服务器装它没有意义。POLARIS_WITH_DESKTOP=1 时安装，
# 或 --with-desktop 参数。
WITH_DESKTOP="${POLARIS_WITH_DESKTOP:-0}"
for arg in "$@"; do
    [ "$arg" = "--with-desktop" ] && WITH_DESKTOP=1
done

if [ "$WITH_DESKTOP" = "1" ]; then
    DESKTOP_NAME="polaris-desktop-${OS}-${ARCH}"
    case "$OS" in
        darwin)  DESKTOP_ARCHIVE="${DESKTOP_NAME}.tar.gz" ;;
        linux)   DESKTOP_ARCHIVE="${DESKTOP_NAME}.AppImage" ;;
        *)       DESKTOP_ARCHIVE="" ;;
    esac

    if [ -z "$DESKTOP_ARCHIVE" ]; then
        msg "⏭️  本平台暂无桌面外壳产物，已跳过。" "⏭️  No desktop bundle for this platform, skipped."
    else
        msg "🖥️  正在安装桌面外壳..." "🖥️  Installing desktop shell..."
        DESKTOP_TMP="$(mktemp -d)"
        DESKTOP_URL="${GITHUB_BASE}/${DESKTOP_ARCHIVE}"
        if curl -sSLf --max-time 300 -o "${DESKTOP_TMP}/${DESKTOP_ARCHIVE}" "$DESKTOP_URL"; then
            # 与二进制同样的完整性校验：下载 .sha256 后比对，失败即放弃安装外壳
            # （但不影响已装好的守护进程——外壳只是界面）。
            if curl -sSLf --max-time 30 -o "${DESKTOP_TMP}/${DESKTOP_ARCHIVE}.sha256" "${DESKTOP_URL}.sha256"; then
                EXPECT=$(awk '{print $1}' "${DESKTOP_TMP}/${DESKTOP_ARCHIVE}.sha256")
                ACTUAL=$(openssl dgst -sha256 "${DESKTOP_TMP}/${DESKTOP_ARCHIVE}" | awk '{print $NF}')
                if [ "$EXPECT" != "$ACTUAL" ]; then
                    msg "⚠️  桌面外壳校验失败，已跳过安装。" "⚠️  Desktop bundle checksum mismatch, skipped."
                    rm -rf "$DESKTOP_TMP"
                    DESKTOP_ARCHIVE=""
                fi
            else
                msg "⚠️  桌面外壳校验和不可得，已跳过安装。" "⚠️  Desktop checksum unavailable, skipped."
                rm -rf "$DESKTOP_TMP"
                DESKTOP_ARCHIVE=""
            fi
        else
            msg "⚠️  桌面外壳下载失败，已跳过（守护进程不受影响）。" \
                "⚠️  Desktop download failed, skipped (daemon unaffected)."
            rm -rf "$DESKTOP_TMP"
            DESKTOP_ARCHIVE=""
        fi

        if [ -n "$DESKTOP_ARCHIVE" ] && [ "$OS" = "darwin" ]; then
            tar -xzf "${DESKTOP_TMP}/${DESKTOP_ARCHIVE}" -C "$DESKTOP_TMP"
            APP_PATH=$(find "$DESKTOP_TMP" -maxdepth 1 -name '*.app' | head -1)
            if [ -n "$APP_PATH" ]; then
                rm -rf "/Applications/$(basename "$APP_PATH")"
                cp -R "$APP_PATH" /Applications/
                # ad-hoc 签名 + 清隔离属性：Apple Silicon 要求有签名，而本脚本
                # 走 curl 下载不会写隔离属性，二者合起来即可双击直接运行。
                codesign --force --deep --sign - "/Applications/$(basename "$APP_PATH")" 2>/dev/null || true
                xattr -dr com.apple.quarantine "/Applications/$(basename "$APP_PATH")" 2>/dev/null || true
                msg "✅ 桌面外壳已安装到 /Applications/$(basename "$APP_PATH")" \
                    "✅ Desktop shell installed to /Applications/$(basename "$APP_PATH")"
            fi
        elif [ -n "$DESKTOP_ARCHIVE" ] && [ "$OS" = "linux" ]; then
            mkdir -p "$HOME/.local/bin"
            install -m 0755 "${DESKTOP_TMP}/${DESKTOP_ARCHIVE}" "$HOME/.local/bin/polaris-desktop"
            msg "✅ 桌面外壳已安装：~/.local/bin/polaris-desktop" \
                "✅ Desktop shell installed: ~/.local/bin/polaris-desktop"
        fi
        rm -rf "$DESKTOP_TMP"
    fi
fi

# ── 7. PATH 提示 ──────────────────────────────────────────────────────────────
echo ""
# 地址取自守护进程自己报告的实际端口——配置 port = 0 时它由内核分配，
# 写死 28888 会把用户指向一个不存在的地址。
CONSOLE_URL=$("${INSTALL_DIR}/${BIN_NAME}" service status --json 2>/dev/null \
    | sed -n 's/.*"base_url": *"\([^"]*\)".*/\1/p')
[ -z "$CONSOLE_URL" ] && CONSOLE_URL="http://127.0.0.1:${PORT}"
msg "🎉 安装完成！请访问 ${CONSOLE_URL} 打开控制台。" \
    "🎉 Installation complete! Visit ${CONSOLE_URL} to open the console."
msg "💡 若需命令行直接使用 polaris，请将以下路径加入 PATH：" \
    "💡 To use polaris in CLI, add to PATH:"
echo "   export PATH=\"\$PATH:${INSTALL_DIR}\""
