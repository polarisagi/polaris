#!/usr/bin/env bash
# -e: 任何命令失败立即退出；-u: 引用未定义变量报错；-o pipefail: 管道任一步骤失败即报错
set -euo pipefail

# 全局系统参数与路径配置（与 internal/config/system_params.go 保持单一事实源一致）
REPO="polarisagi/polaris"
BIN_NAME="polaris"
INSTALL_DIR="$HOME/.polaris/bin"
DATA_DIR="$HOME/.polaris"
DEPS_DIR="$DATA_DIR/deps"
LOCAL_BIN="$HOME/.local/bin"
PLIST_LABEL="com.polarisagi.polaris"
PLIST_PATH="$HOME/Library/LaunchAgents/${PLIST_LABEL}.plist"
PORT=28888

# 自动安装固定主版本，补丁版本由官方 SHASUMS 清单选择。
NODE_VERSION="24"

# 使用 ${VAR:-} 防止全新系统未设置 LC_ALL / LANGUAGE 时因 set -u 直接崩溃
if [[ "${LANG:-}" == *"zh"* ]] || [[ "${LC_ALL:-}" == *"zh"* ]] || [[ "${LANGUAGE:-}" == *"zh"* ]]; then
    LANG_ZH=true
else
    LANG_ZH=false
fi

msg() {
    if [ "$LANG_ZH" = true ]; then echo "$1"; else echo "$2"; fi
}

msg "🌌 正在安装/更新 PolarisAGI Polaris..." \
    "🌌 Installing/Updating PolarisAGI Polaris..."

# 提前创建并导出用户级工具目录至 PATH 最前端：
# 1. 确保 ~/.local/bin 排在 /usr/bin 前面，屏蔽全新 macOS 未装 CLT 时的 /usr/bin/python3 空壳桩程序
# 2. 确保后续 polaris service install 采样当前 shell PATH 时包含 ~/.local/bin 与 ~/.polaris/bin
mkdir -p "$LOCAL_BIN" "$INSTALL_DIR" "$DEPS_DIR"
export PATH="$LOCAL_BIN:$HOME/.cargo/bin:$INSTALL_DIR:$PATH"

OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)

case "$OS" in
    darwin|linux) ;;
    *) msg "❌ 不支持的平台: $OS" "❌ Unsupported platform: $OS"; exit 1 ;;
esac

# 隔离并发安装和残留分片，避免固定 /tmp 文件被串用或被符号链接替换。
WORK_DIR=$(mktemp -d "${TMPDIR:-/tmp}/polaris-install.XXXXXX")
trap 'rm -rf "$WORK_DIR"' EXIT
DEPS_READY=true
SERVICE_READY=false

# 发布包的 Rust 动态库使用 glibc，不能在 Alpine/musl 上假装安装成功。
if [ "$OS" = linux ] && [ -f /etc/alpine-release ]; then
    msg "❌ 当前发布包不支持 Alpine/musl，请使用 glibc Linux。" "❌ Release binaries require glibc Linux; Alpine/musl is unsupported."
    exit 1
fi

case "$ARCH" in
    x86_64)          ARCH="amd64" ;;
    aarch64|arm64)   ARCH="arm64" ;;
    *)
        msg "❌ 不支持的架构: $ARCH" "❌ Unsupported architecture: $ARCH"
        exit 1
        ;;
esac

# ── 0.1 Linux 最小化系统基础包预检 (curl / tar / openssl / git) ─────────────
# 在全新 Linux（如最小化 Debian/Ubuntu/CentOS 容器或 VPS）上，甚至可能缺少
# curl / tar / openssl / ca-certificates / git。必须在网络探测（依赖 curl）前补齐。
install_linux_base_packages() {
    if [ "$OS" != "linux" ]; then
        return 0
    fi
    local missing=()
    for cmd in curl tar gzip openssl git bwrap; do
        if ! command -v "$cmd" >/dev/null 2>&1; then
            missing+=("$cmd")
        fi
    done
    if [ ! -s /etc/ssl/certs/ca-certificates.crt ] && [ ! -s /etc/pki/tls/certs/ca-bundle.crt ]; then
        missing+=("ca-certificates")
    fi
    if [ ${#missing[@]} -eq 0 ]; then
        return 0
    fi

    msg "📦 检测到缺少系统基础工具 (${missing[*]})，正在通过包管理器自动安装..." \
        "📦 Missing base tools (${missing[*]}), installing via system package manager..."

    local sudo_cmd=""
    if [ "$(id -u)" -ne 0 ]; then
        if command -v sudo >/dev/null 2>&1; then
            sudo_cmd="sudo"
        else
            msg "⚠️  当前非 root 用户且未找到 sudo，无法自动安装系统包: ${missing[*]}" \
                "⚠️  Not running as root and sudo not found; cannot auto-install: ${missing[*]}"
            return 1
        fi
    fi

    if command -v apt-get >/dev/null 2>&1; then
        $sudo_cmd env DEBIAN_FRONTEND=noninteractive apt-get update -y >/dev/null 2>&1 || true
        $sudo_cmd env DEBIAN_FRONTEND=noninteractive apt-get install -y curl tar gzip openssl ca-certificates || true
        $sudo_cmd env DEBIAN_FRONTEND=noninteractive apt-get install -y git || true
        $sudo_cmd env DEBIAN_FRONTEND=noninteractive apt-get install -y bubblewrap || true
    elif command -v dnf >/dev/null 2>&1; then
        $sudo_cmd dnf install -y curl tar gzip openssl ca-certificates || true
        $sudo_cmd dnf install -y git || true
        $sudo_cmd dnf install -y bubblewrap || true
    elif command -v yum >/dev/null 2>&1; then
        $sudo_cmd yum install -y curl tar gzip openssl ca-certificates || true
        $sudo_cmd yum install -y git || true
        $sudo_cmd yum install -y bubblewrap || true
    elif command -v pacman >/dev/null 2>&1; then
        $sudo_cmd pacman -S --noconfirm --needed curl tar gzip openssl ca-certificates || true
        $sudo_cmd pacman -S --noconfirm --needed git || true
        $sudo_cmd pacman -S --noconfirm --needed bubblewrap || true
    elif command -v apk >/dev/null 2>&1; then
        $sudo_cmd apk add --no-cache curl tar gzip openssl ca-certificates || true
        $sudo_cmd apk add --no-cache git || true
        $sudo_cmd apk add --no-cache bubblewrap || true
    elif command -v zypper >/dev/null 2>&1; then
        $sudo_cmd zypper --non-interactive install curl tar gzip openssl ca-certificates || true
        $sudo_cmd zypper --non-interactive install git || true
        $sudo_cmd zypper --non-interactive install bubblewrap || true
    fi
}

install_linux_base_packages || true

for req_cmd in curl tar gzip openssl; do
    if ! command -v "$req_cmd" >/dev/null 2>&1; then
        msg "❌ 缺少必需的系统基础命令: $req_cmd。请先通过系统包管理器安装后重试。" \
            "❌ Required base command not found: $req_cmd. Please install it via your package manager and retry."
        exit 1
    fi
done

# ── 0.2 确定网络环境 (Network Detection) ─────────────────────────────────────
PROXY_HOSTS=("https://ghproxy.net" "https://mirror.ghproxy.com")
GH_PROXY=""
IS_CN=false

# 判断当前网络环境是否处于中国大陆
is_mainland_china() {
    local timeout=2
    local country=""
    
    # 尝试 1: ipinfo.io
    country=$(curl -sSf -m $timeout https://ipinfo.io/country 2>/dev/null | tr -d '[:space:]' || true)
    if [ -n "$country" ]; then
        [ "$country" = "CN" ] && return 0 || return 1
    fi
    
    # 尝试 2: cloudflare trace
    local cf_trace
    cf_trace=$(curl -sSf -m $timeout https://1.1.1.1/cdn-cgi/trace 2>/dev/null || true)
    if [ -n "$cf_trace" ] && echo "$cf_trace" | grep -q "loc="; then
        if echo "$cf_trace" | grep -q "loc=CN"; then
            return 0
        else
            return 1
        fi
    fi
    
    # 尝试 3: ip.sb
    local ipsb
    ipsb=$(curl -sSf -m $timeout https://api.ip.sb/geoip 2>/dev/null || true)
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
    IS_CN=true
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

# 校验 git 是否真实可用：
# 全新 macOS 自带 /usr/bin/git 空壳桩程序（Xcode Command Line Tools 未安装时），
# command -v git 会返回 0，但实际执行 git 会弹窗并报错退出。
has_working_git() {
    local git_path
    git_path=$(command -v git 2>/dev/null || true)
    if [ -z "$git_path" ]; then
        return 1
    fi
    if [ "$OS" = "darwin" ] && [ "$git_path" = "/usr/bin/git" ] && ! xcode-select -p >/dev/null 2>&1; then
        return 1
    fi
    "$git_path" --version >/dev/null 2>&1
}

# 校验 python3 是否真实可用（同样排除 macOS 未装 CLT 时的 /usr/bin/python3 空壳桩程序）
has_working_python() {
    local py_bin
    py_bin=$(command -v python3 2>/dev/null || true)
    if [ -z "$py_bin" ]; then
        return 1
    fi
    if [ "$OS" = "darwin" ] && [ "$py_bin" = "/usr/bin/python3" ] && ! xcode-select -p >/dev/null 2>&1; then
        return 1
    fi
    "$py_bin" -c "import sys; sys.exit(0 if sys.version_info.major >= 3 else 1)" >/dev/null 2>&1
}

# 校验 node 与 npx 是否真实可用
has_working_node() {
    node -e 'process.exit(Number(process.versions.node.split(".")[0]) >= 22 ? 0 : 1)' >/dev/null 2>&1 &&
        npm --version >/dev/null 2>&1 && npx --version >/dev/null 2>&1
}

# 1. Git 检查与自动安装
MACOS_CLT_TRIGGERED=false
if ! has_working_git; then
    if [ "$OS" = "darwin" ]; then
        if command -v brew >/dev/null 2>&1; then
            msg "   📦 正在通过 Homebrew 安装 Git..." "   📦 Installing Git via Homebrew..."
            brew install git || true
        fi
        if ! has_working_git; then
            msg "⚠️  macOS 尚未安装 Git (Command Line Tools)。正在后台唤起系统安装向导..." \
                "⚠️  macOS is missing Git (Command Line Tools). Triggering system install prompt..."
            xcode-select --install 2>/dev/null || true
            MACOS_CLT_TRIGGERED=true
            msg "💡 请在弹出的系统窗口中点击「安装」；本脚本不依赖 Git，将继续完成其余环境与 Polaris 安装。" \
                "💡 Please click 'Install' in the system dialog; this script will continue installing dependencies and Polaris."
        fi
    else
        msg "⚠️  未能自动安装 Git，部分扩展仓库克隆功能需要 Git，请稍后通过系统包管理器安装 git。" \
            "⚠️  Could not auto-install Git. Some extension features require Git; please install it via your package manager later."
    fi
fi

# 2. uv (跨平台 Python 工具链与包管理器)
if ! uv --version >/dev/null 2>&1 || ! uvx --version >/dev/null 2>&1; then
    msg "   📦 正在安装 uv..." "   📦 Installing uv..."
    UV_INSTALLED=false
    if [ -n "$GH_PROXY" ]; then
        if curl -LsSf --max-time 60 https://astral.sh/uv/install.sh | env UV_INSTALL_DIR="$LOCAL_BIN" UV_NO_MODIFY_PATH=1 UV_INSTALLER_GITHUB_BASE_URL="${GH_PROXY%/}/https://github.com" sh; then
            UV_INSTALLED=true
        fi
    fi
    if [ "$UV_INSTALLED" = false ]; then
        if ! curl -LsSf --max-time 60 https://astral.sh/uv/install.sh | env UV_INSTALL_DIR="$LOCAL_BIN" UV_NO_MODIFY_PATH=1 sh; then
            msg "⚠️ uv 安装失败，依赖检查将报告缺失。" "⚠️ uv installation failed; see dependency checks below."
        fi
    fi
fi

# 确保 uv / uvx 位于 ~/.local/bin 下（旧版 uv 可能装在 ~/.cargo/bin）
for uv_tool in uv uvx; do
    if [ ! -e "$LOCAL_BIN/$uv_tool" ] && [ -x "$HOME/.cargo/bin/$uv_tool" ]; then
        ln -sf "$HOME/.cargo/bin/$uv_tool" "$LOCAL_BIN/$uv_tool"
    fi
done

# 3. Python 3 (通过 uv 独立安装，无需编译器或 Xcode CLT)
if ! has_working_python && uv --version >/dev/null 2>&1; then
    msg "   🐍 正在通过 uv 安装独立 Python 3 环境..." "   🐍 Installing standalone Python 3 via uv..."
    PY_INSTALLED=false
    if [ -n "$GH_PROXY" ]; then
        if UV_PYTHON_INSTALL_MIRROR="${GH_PROXY}https://github.com/astral-sh/python-build-standalone/releases/download" uv python install 3.12; then
            PY_INSTALLED=true
        fi
    fi
    if [ "$PY_INSTALLED" = false ]; then
        uv python install 3.12 || true
    fi

    # 将 uv 安装的 python 软链接到 ~/.local/bin（排在 /usr/bin 前面，覆盖 macOS 系统桩程序）
    UV_PYTHON_BIN=$(uv python find --managed-python 3.12 2>/dev/null || true)
    if [ -n "$UV_PYTHON_BIN" ] && [ -x "$UV_PYTHON_BIN" ]; then
        ln -sf "$UV_PYTHON_BIN" "$LOCAL_BIN/python3"
        ln -sf "$UV_PYTHON_BIN" "$LOCAL_BIN/python"
    fi
fi

# 若系统已有可用 python3 但无 python 别名，补齐软链接
if has_working_python && ! command -v python >/dev/null 2>&1; then
    ln -sf "$(command -v python3)" "$LOCAL_BIN/python"
fi

# 4. Node.js (优先使用官方/国内镜像预编译包直装到 ~/.polaris/deps/node 并软链至 ~/.local/bin；
#    不依赖 git、不触发 nvm.sh 的 set -u 崩溃，新终端与后台服务均可直接寻址)
export NVM_DIR="$HOME/.nvm"
if ! has_working_node && [ -s "$NVM_DIR/nvm.sh" ]; then
    # nvm.sh 内部引用未定义变量，必须临时关闭 -u / -e
    set +eu
    \. "$NVM_DIR/nvm.sh" >/dev/null 2>&1
    set -euo pipefail
fi

install_node_prebuilt() {
    local node_os="$OS"
    local node_arch="x64"
    if [ "$ARCH" = "arm64" ]; then
        node_arch="arm64"
    fi

    local mirrors=()
    if [ "$IS_CN" = true ]; then
        mirrors=("https://npmmirror.com/mirrors/node/latest-v${NODE_VERSION}.x" "https://nodejs.org/dist/latest-v${NODE_VERSION}.x")
    else
        mirrors=("https://nodejs.org/dist/latest-v${NODE_VERSION}.x" "https://npmmirror.com/mirrors/node/latest-v${NODE_VERSION}.x")
    fi

    local tmp_node_archive="$WORK_DIR/node.tar.gz"
    for base_url in "${mirrors[@]}"; do
        local shasums
        shasums=$(curl -sSLf --max-time 15 "https://nodejs.org/dist/latest-v${NODE_VERSION}.x/SHASUMS256.txt" 2>/dev/null || true)
        if [ -z "$shasums" ]; then
            continue
        fi
        local line
        line=$(echo "$shasums" | grep -E "node-v${NODE_VERSION}\.[0-9]+\.[0-9]+-${node_os}-${node_arch}\.tar\.gz$" | head -1 || true)
        if [ -z "$line" ]; then
            continue
        fi
        local expected_sha
        local filename
        expected_sha=$(echo "$line" | awk '{print $1}')
        filename=$(echo "$line" | awk '{print $2}')

        msg "   ⬇️  正在下载 Node.js (${filename})..." "   ⬇️  Downloading Node.js (${filename})..."
        if curl -sSLf --progress-bar --max-time 180 -o "$tmp_node_archive" "${base_url}/${filename}"; then
            local actual_sha
            actual_sha=$(openssl dgst -sha256 "$tmp_node_archive" | awk '{print $NF}')
            if [ "$expected_sha" = "$actual_sha" ]; then
                rm -rf "$DEPS_DIR/node"
                mkdir -p "$DEPS_DIR/node"
                tar -xzf "$tmp_node_archive" -C "$DEPS_DIR/node" --strip-components=1
                rm -f "$tmp_node_archive"
                for bin_item in node npm npx corepack; do
                    if [ -e "$DEPS_DIR/node/bin/$bin_item" ]; then
                        ln -sf "$DEPS_DIR/node/bin/$bin_item" "$LOCAL_BIN/$bin_item"
                    fi
                done
                hash -r
                has_working_node && return 0
            fi
        fi
        rm -f "$tmp_node_archive"
    done
    return 1
}

if ! has_working_node; then
    msg "   🟢 正在安装 Node.js v${NODE_VERSION}..." "   🟢 Installing Node.js v${NODE_VERSION}..."
    if ! install_node_prebuilt; then
        msg "⚠️ Node.js 预编译包安装失败，请检查网络或系统兼容性。" \
            "⚠️ Node.js archive installation failed. Check network and OS compatibility."
    fi
fi

# 无论 Node 来自 nvm 还是预编译包，确保 node / npm / npx 在 ~/.local/bin 中有软链接，
# 使 launchd / systemd / 沙箱 / 非交互式子进程无需 source nvm.sh 即可直接调用。
if has_working_node; then
    NODE_BIN_DIR=$(dirname "$(command -v node)")
    if [ "$NODE_BIN_DIR" != "$LOCAL_BIN" ] && [ "$NODE_BIN_DIR" != "/usr/bin" ] && [ "$NODE_BIN_DIR" != "/usr/local/bin" ]; then
        for bin_item in node npm npx corepack; do
            if [ -e "$NODE_BIN_DIR/$bin_item" ]; then
                ln -sf "$NODE_BIN_DIR/$bin_item" "$LOCAL_BIN/$bin_item"
            fi
        done
    fi
    # 大陆网络下为 npm 配置国内镜像源，防止后续 MCP npx 拉包超时
    if [ "$IS_CN" = true ] && command -v npm >/dev/null 2>&1; then
        CURRENT_NPM_REG=$(npm config get registry 2>/dev/null || true)
        if [[ "$CURRENT_NPM_REG" == *"registry.npmjs.org"* ]]; then
            npm config set registry https://registry.npmmirror.com >/dev/null 2>&1 || true
        fi
    fi
fi

# 安装器退出码不代表工具可用，后台服务需要这些命令实际执行成功。
for dep in git python uv node; do
    case "$dep" in
        git) has_working_git && continue ;;
        python) has_working_python && continue ;;
        uv) uv --version >/dev/null 2>&1 && uvx --version >/dev/null 2>&1 && continue ;;
        node) has_working_node && continue ;;
    esac
    DEPS_READY=false
    msg "⚠️ 运行环境尚未就绪: $dep" "⚠️ Dependency not ready: $dep"
done
if [ "$DEPS_READY" = true ]; then
    msg "✅ 基础环境准备就绪。" "✅ Dependencies ready."
fi
if [ "$OS" = linux ] && ! command -v bwrap >/dev/null 2>&1; then
    DEPS_READY=false
    msg "⚠️ 缺少 bubblewrap，隔离执行不可用，请通过系统包管理器安装。" \
        "⚠️ bubblewrap is missing; isolated execution is unavailable. Install it with your package manager."
fi

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

# ── 2. 下载（逐源重试）──────────────────────────────────────────
TMP_ARCHIVE="$WORK_DIR/polaris.tar.gz"
TMP_DIR="$WORK_DIR/extracted"

msg "⬇️  开始下载（逐源重试）..." "⬇️  Downloading (with source retries)..."

DOWNLOADED=false
for URL in "${CANDIDATE_URLS[@]}"; do
    msg "   尝试: $URL" "   Trying: $URL"
    rm -f "$TMP_ARCHIVE"
    if curl -sSLf --retry 2 --progress-bar --max-time 300 -o "$TMP_ARCHIVE" "$URL"; then
        DOWNLOADED=true
        break
    else
        EXIT_CODE=$?
        msg "   此源失败（exit ${EXIT_CODE}），尝试下一个..." \
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
    if [ "${POLARIS_ALLOW_MIRROR_CHECKSUM:-0}" = "1" ] && [ "$URL" != "$DIRECT_URL" ]; then
        msg "⚠️  无法从官方直连源获取校验和文件。检测到 POLARIS_ALLOW_MIRROR_CHECKSUM=1，" \
            "⚠️  Failed to fetch checksum from the official source. POLARIS_ALLOW_MIRROR_CHECKSUM=1 detected,"
        msg "⚠️  按你的显式授权回退到二进制同源（${URL}）下载校验和。此模式下无法防御该源同时" \
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
if [ ! -f "$TMP_DIR/$BIN_NAME" ]; then
    msg "❌ 归档缺少程序文件，旧安装未改动。" "❌ Binary missing from archive; existing installation preserved."
    exit 1
fi
chmod +x "$TMP_DIR/$BIN_NAME"
if ! "$TMP_DIR/$BIN_NAME" version >/dev/null 2>&1; then
    msg "❌ 新程序无法在此系统运行，旧安装未改动。" "❌ New binary cannot run on this OS; existing installation preserved."
    exit 1
fi

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
    SERVICE_READY=true
    msg "✅ 服务已注册并启动，随登录自动运行。" \
        "✅ Service registered and started, auto-starts on login."
else
    msg "⚠️ 服务注册失败。可手动运行 ${INSTALL_DIR}/${BIN_NAME} serve；有用户服务会话后再执行 service install。" \
        "⚠️ Service registration failed. Run ${INSTALL_DIR}/${BIN_NAME} serve manually, or retry service install with a user service session."
fi

# ── 6.8 桌面外壳安装 ─────────────────────────────────────────────────────────
# 统一归档内含 desktop/ 目录（有桌面版的平台才存在）。安装脚本将桌面外壳放到
# 系统常规位置，使其可被 Finder / 应用启动器发现。守护进程与桌面外壳同包分发，
# 不再单独下载（ADR-0096 决策四的"分开安装"指运行时分离，不指分发渠道）。
DESKTOP_SRC="${INSTALL_DIR}/desktop"
if [ -d "$DESKTOP_SRC" ]; then
    if [ "$OS" = "darwin" ]; then
        APP_PATH=$(find "$DESKTOP_SRC" -maxdepth 1 -name '*.app' | head -1)
        if [ -n "$APP_PATH" ]; then
            APP_NAME=$(basename "$APP_PATH")
            # 普通账户可能无权写 /Applications，使用可写的用户应用目录。
            APP_ROOT="/Applications"
            if [ ! -w "$APP_ROOT" ]; then
                APP_ROOT="$HOME/Applications"
                mkdir -p "$APP_ROOT"
            fi
            rm -rf "$APP_ROOT/${APP_NAME}"
            cp -R "$APP_PATH" "$APP_ROOT/${APP_NAME}"
            codesign --force --deep --sign - "$APP_ROOT/${APP_NAME}" 2>/dev/null || true
            xattr -dr com.apple.quarantine "$APP_ROOT/${APP_NAME}" 2>/dev/null || true
            msg "✅ 桌面外壳已安装到 $APP_ROOT/${APP_NAME}" \
                "✅ Desktop shell installed to $APP_ROOT/${APP_NAME}"
        fi
    elif [ "$OS" = "linux" ]; then
        APPIMAGE=$(find "$DESKTOP_SRC" -maxdepth 1 -name '*.AppImage' | head -1)
        if [ -n "$APPIMAGE" ]; then
            mkdir -p "$HOME/.local/bin"
            install -m 0755 "$APPIMAGE" "$HOME/.local/bin/polaris-desktop"
            # 创建 .desktop 文件供应用启动器发现
            mkdir -p "$HOME/.local/share/applications"
            cat > "$HOME/.local/share/applications/polaris-desktop.desktop" <<DESKTOP
[Desktop Entry]
Type=Application
Name=Polaris
Comment=Polaris AI Agent Desktop
Exec="$HOME/.local/bin/polaris-desktop"
Icon=polaris
Terminal=false
Categories=Development;
DESKTOP
            msg "✅ 桌面外壳已安装：~/.local/bin/polaris-desktop" \
                "✅ Desktop shell installed: ~/.local/bin/polaris-desktop"
        fi
    fi
    # 已复制到系统常规位置，bin/ 下的解压原件不再需要，清掉避免永久占用磁盘
    rm -rf "$DESKTOP_SRC"
fi

# ── 7. 持久化 Shell PATH 与完成提示 ───────────────────────────────────────────
# 全新 macOS 默认无 ~/.zshrc，全新 Linux 默认不一定将 ~/.local/bin 与 ~/.polaris/bin
# 纳入 PATH。幂等写入用户 shell 配置文件，确保新开终端直接可用 polaris / node / python3 / uv。
ensure_shell_path() {
    local marker="# >>> polaris path >>>"
    local path_line='export PATH="$HOME/.polaris/bin:$HOME/.local/bin:$PATH"'
    local targets=()

    if [ "$OS" = "darwin" ] || [[ "${SHELL:-}" == *"zsh"* ]] || [ -f "$HOME/.zshrc" ]; then
        targets+=("$HOME/.zshrc" "$HOME/.zprofile")
    fi
    if [[ "${SHELL:-}" == *"bash"* ]] || [ -f "$HOME/.bashrc" ]; then
        targets+=("$HOME/.bashrc")
        if [ -f "$HOME/.bash_profile" ]; then
            targets+=("$HOME/.bash_profile")
        elif [ -f "$HOME/.bash_login" ]; then
            targets+=("$HOME/.bash_login")
        else
            targets+=("$HOME/.profile")
        fi
    fi
    if [ ${#targets[@]} -eq 0 ]; then
        targets+=("$HOME/.profile")
    fi

    for rc in "${targets[@]}"; do
        if [ ! -f "$rc" ] || ! grep -Fq "$marker" "$rc" 2>/dev/null; then
            {
                echo ""
                echo "$marker"
                echo "$path_line"
                echo "# <<< polaris path <<<"
            } >> "$rc" 2>/dev/null || true
        fi
    done
}

ensure_shell_path

echo ""
# 地址取自守护进程自己报告的实际端口——配置 port = 0 时它由内核分配，
# 写死 28888 会把用户指向一个不存在的地址。
CONSOLE_URL=""
if [ "$SERVICE_READY" = true ]; then
    SERVICE_READY=false
    for ((attempt = 0; attempt < 30; attempt++)); do
        CONSOLE_URL=$("${INSTALL_DIR}/${BIN_NAME}" service status --json 2>/dev/null \
            | sed -n 's/.*"base_url": *"\([^"]*\)".*/\1/p' || true)
        if [ -n "$CONSOLE_URL" ]; then
            SERVICE_READY=true
            break
        fi
        sleep 1
    done
fi
[ -z "$CONSOLE_URL" ] && CONSOLE_URL="http://127.0.0.1:${PORT}"
if [ "$DEPS_READY" = true ] && [ "$SERVICE_READY" = true ]; then
    msg "🎉 安装完成！控制台地址: ${CONSOLE_URL}" "🎉 Installation complete! Console: ${CONSOLE_URL}"
else
    msg "⚠️ 主程序已安装，但依赖或后台服务未就绪，请按上方提示处理后重跑安装。" \
        "⚠️ Core installed, but dependencies or service need attention. Follow the messages above and rerun."
fi
if [ "$MACOS_CLT_TRIGGERED" = true ]; then
    msg "⚠️  提示：若 macOS 弹出了「命令行开发者工具 (Command Line Tools)」安装窗口，请点击「安装」以完成 Git 配置。" \
        "⚠️  Note: If macOS prompted to install Command Line Developer Tools, please complete it to enable Git."
fi
msg "💡 已自动将 PATH 写入 Shell 配置文件，新开终端即可直接使用 polaris。当前终端立即生效可执行：" \
    "💡 PATH has been added to your shell profile. To use polaris in the current terminal immediately, run:"
echo "   export PATH=\"\$HOME/.polaris/bin:\$HOME/.local/bin:\$PATH\""

[ "$DEPS_READY" = true ] && [ "$SERVICE_READY" = true ]
