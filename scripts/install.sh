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

# Pin 到经官方摘要核验的补丁版，避免安装时额外依赖动态摘要服务。
NODE_VERSION="24.22.3"
UV_VERSION="0.13.7"

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

if [ -n "${POLARIS_DOWNLOAD_BASE:-}" ] && [[ ! "$POLARIS_DOWNLOAD_BASE" =~ ^https://[^/?#[:space:]]+(/[^?#[:space:]]*)?$ ]]; then
    msg "❌ POLARIS_DOWNLOAD_BASE 必须是有效的 HTTPS 基础目录。" "❌ POLARIS_DOWNLOAD_BASE must be a valid HTTPS base URL."
    exit 1
fi

# ── 0.1 Linux 最小化系统基础包预检 (curl / tar / openssl / git) ─────────────
# 在全新 Linux（如最小化 Debian/Ubuntu/CentOS 容器或 VPS）上，甚至可能缺少
# curl / tar / openssl / ca-certificates / git。必须在网络探测（依赖 curl）前补齐。
install_linux_base_packages() {
    if [ "$OS" != "linux" ]; then
        return 0
    fi
    local missing=() packages=()
    for cmd in curl tar gzip unzip openssl git bwrap; do
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
    for cmd in "${missing[@]}"; do
        case "$cmd" in bwrap) packages+=(bubblewrap) ;; *) packages+=("$cmd") ;; esac
    done

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
        $sudo_cmd env DEBIAN_FRONTEND=noninteractive apt-get install -y "${packages[@]}" || true
    elif command -v dnf >/dev/null 2>&1; then
        $sudo_cmd dnf install -y "${packages[@]}" || true
    elif command -v yum >/dev/null 2>&1; then
        $sudo_cmd yum install -y "${packages[@]}" || true
    elif command -v pacman >/dev/null 2>&1; then
        $sudo_cmd pacman -S --noconfirm --needed "${packages[@]}" || true
    elif command -v apk >/dev/null 2>&1; then
        $sudo_cmd apk add --no-cache "${packages[@]}" || true
    elif command -v zypper >/dev/null 2>&1; then
        $sudo_cmd zypper --non-interactive install "${packages[@]}" || true
    fi
}

install_linux_base_packages || true

for req_cmd in curl tar gzip unzip openssl; do
    if ! command -v "$req_cmd" >/dev/null 2>&1; then
        msg "❌ 缺少必需的系统基础命令: $req_cmd。请先通过系统包管理器安装后重试。" \
            "❌ Required base command not found: $req_cmd. Please install it via your package manager and retry."
        exit 1
    fi
done

REGION="${POLARIS_REGION:-auto}"
case "$REGION" in auto|cn|global) ;; *) msg "❌ POLARIS_REGION 必须为 auto/cn/global" "❌ POLARIS_REGION must be auto/cn/global"; exit 1 ;; esac


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
    local entry py_bin
    local -a path_entries
    IFS=: read -r -a path_entries <<< "$PATH"
    for entry in "${path_entries[@]}"; do
        [ -n "$entry" ] || entry=.
        py_bin="$entry/python3"
        [ -x "$py_bin" ] || continue
        if [ "$OS" = darwin ] && [ "$py_bin" = "/usr/bin/python3" ] && ! xcode-select -p >/dev/null 2>&1; then continue; fi
        if "$py_bin" -c "import sys; sys.exit(0 if sys.version_info.major >= 3 else 1)" >/dev/null 2>&1; then
            WORKING_PYTHON="$py_bin"
            PATH="$entry:$PATH"; export PATH
            return 0
        fi
    done
    return 1
}

# 校验 node 与 npx 是否真实可用
has_working_node() {
    local entry node_bin
    local -a path_entries
    IFS=: read -r -a path_entries <<< "$PATH"
    for entry in "${path_entries[@]}"; do
        [ -n "$entry" ] || entry=.
        node_bin="$entry/node"
        [ -x "$node_bin" ] || continue
        if "$node_bin" -e 'process.exit(Number(process.versions.node.split(".")[0]) >= 22 ? 0 : 1)' >/dev/null 2>&1 &&
           [ -x "$entry/npm" ] && "$entry/npm" --version >/dev/null 2>&1 &&
           [ -x "$entry/npx" ] && "$entry/npx" --version >/dev/null 2>&1; then
            WORKING_NODE_DIR="$entry"
            PATH="$entry:$PATH"; export PATH
            return 0
        fi
    done
    return 1
}

has_working_uv() {
    local entry
    local -a path_entries
    IFS=: read -r -a path_entries <<< "$PATH"
    for entry in "${path_entries[@]}"; do
        [ -n "$entry" ] || entry=.
        if [ -x "$entry/uv" ] && [ -x "$entry/uvx" ] &&
           "$entry/uv" --version >/dev/null 2>&1 && "$entry/uvx" --version >/dev/null 2>&1; then
            PATH="$entry:$PATH"; export PATH
            return 0
        fi
    done
    return 1
}

# 包管理器维护依赖与摘要；不替用户安装另一个包管理器或改系统源。
install_dependency_package() {
    local brew_name="$1" port_name="$2" linux_name="$3"
    local sudo_cmd="" manager="" prefix=""
    if [ "$OS" = darwin ]; then
        if command -v brew >/dev/null 2>&1 && brew install "$brew_name"; then
            prefix=$(brew --prefix 2>/dev/null || true)
            [ -z "$prefix" ] || PATH="$prefix/bin:$PATH"
            if [ "$brew_name" = node@24 ] && [ -x "$prefix/opt/node@24/bin/node" ]; then PATH="$prefix/opt/node@24/bin:$PATH"; fi
            export PATH; hash -r; return 0
        fi
        if command -v port >/dev/null 2>&1; then
            if [ "$(id -u)" -ne 0 ]; then command -v sudo >/dev/null 2>&1 || return 1; sudo_cmd=sudo; fi
            if $sudo_cmd port install "$port_name"; then PATH="/opt/local/bin:/opt/local/sbin:$PATH"; export PATH; hash -r; return 0; fi
        fi
        return 1
    fi
    [ "$OS" = linux ] && [ -n "$linux_name" ] || return 1
    if [ "$(id -u)" -ne 0 ]; then command -v sudo >/dev/null 2>&1 || return 1; sudo_cmd="sudo"; fi
    if command -v apt-get >/dev/null 2>&1; then manager=apt-get
    elif command -v dnf >/dev/null 2>&1; then manager=dnf
    elif command -v yum >/dev/null 2>&1; then manager=yum
    elif command -v pacman >/dev/null 2>&1; then manager=pacman
    elif command -v zypper >/dev/null 2>&1; then manager=zypper
    else return 1; fi
    if [ "$manager" = apt-get ]; then
        $sudo_cmd apt-get update || return 1
        $sudo_cmd apt-get install -y $linux_name || return 1
    elif [ "$manager" = pacman ]; then
        $sudo_cmd pacman -S --needed --noconfirm $linux_name || return 1
    elif [ "$manager" = zypper ]; then
        $sudo_cmd zypper --non-interactive install $linux_name || return 1
    else
        $sudo_cmd "$manager" install -y $linux_name || return 1
    fi
    hash -r
}

# GUI 启动的安装器通常拿不到 brew/MacPorts 默认 PATH。
if [ "$OS" = darwin ]; then
    export PATH="$PATH:/opt/homebrew/bin:/usr/local/bin:/opt/local/bin:/opt/local/sbin"
fi
if [ "$OS" = darwin ] && [ ! -x "$LOCAL_BIN/python3" ] && [ -x /opt/local/bin/python3.12 ] && ! has_working_python; then
    ln -s /opt/local/bin/python3.12 "$LOCAL_BIN/python3"
fi
if ! has_working_node; then
    if [ "$OS" = darwin ] && command -v port >/dev/null 2>&1 && command -v node >/dev/null 2>&1 && node -e 'process.exit(Number(process.versions.node.split(".")[0])>=22?0:1)' >/dev/null 2>&1 &&
       ! npm --version >/dev/null 2>&1; then
        if [ "$(id -u)" -eq 0 ]; then port install npm10 || true
        elif command -v sudo >/dev/null 2>&1; then sudo port install npm10 || true; fi
        PATH="/opt/local/bin:$PATH"; export PATH; hash -r
    fi
    if ! has_working_node && ! command -v node >/dev/null 2>&1; then
        install_dependency_package node@24 nodejs24 'nodejs npm' || true
    fi
fi

# 1. Git 检查与自动安装
MACOS_CLT_TRIGGERED=false
if ! has_working_git; then
    if [ "$OS" = "darwin" ]; then
        install_dependency_package git git "" || true
        if ! has_working_git; then
            msg "⚠️  macOS 尚未安装 Git (Command Line Tools)。正在后台唤起系统安装向导..." \
                "⚠️  macOS is missing Git (Command Line Tools). Triggering system install prompt..."
            xcode-select --install 2>/dev/null || true
            MACOS_CLT_TRIGGERED=true
            msg "💡 请在弹出的系统窗口中点击「安装」；完成后重新运行本脚本以继续安装。" \
                "💡 Please click 'Install' in the system dialog, then rerun this script to continue setup."
        fi
    else
        msg "⚠️  未能自动安装 Git，部分扩展仓库克隆功能需要 Git，请稍后通过系统包管理器安装 git。" \
            "⚠️  Could not auto-install Git. Some extension features require Git; please install it via your package manager later."
    fi
fi

# 固定摘要作为信任根，不从下载源动态取值。
fixed_archive_sha256() {
    case "$1" in
        node-v24.22.3-darwin-arm64.tar.gz) echo "ff6e17d4874b76d272212b908cc020d6e815fba5a6c7eaaa06add725840307fd" ;;
        node-v24.22.3-darwin-x64.tar.gz) echo "977614af0504c81045e7fd85596dbca0e5f951b96681516dadafedcca8c447f7" ;;
        node-v24.22.3-linux-arm64.tar.gz) echo "261af2af250dfd7e9f25cf151d1b2713a0adf8f10a9ad116c4947f68627affe8" ;;
        node-v24.22.3-linux-x64.tar.gz) echo "061ec3e3062115705b56b51fa9cc330ab50d34b6969387d4e4b1a50c6809acc2" ;;
        uv-0.13.7-py3-none-macosx_11_0_arm64.whl) echo "f91a229093163eefca89ea1b220a221d64c3beade05d03519a6584d1797a5a5f" ;;
        uv-0.13.7-py3-none-macosx_10_12_x86_64.whl) echo "fd04bf01ba5f2829f4f94ca8f0c24a743f21e73d75045613ed6fc2fd66fc7bda" ;;
        uv-0.13.7-py3-none-manylinux_2_17_aarch64.manylinux2014_aarch64.musllinux_1_1_aarch64.whl) echo "a90f5d95716ab83091f0f73a8e7ec64e557abf2a28537fcd719b2ac88e6f725a" ;;
        uv-0.13.7-py3-none-manylinux_2_17_x86_64.manylinux2014_x86_64.whl) echo "2c9bdc4a872b60a93c5f26c0791089ea11585a4f9d3b3c017d78eb8487505e25" ;;
        *) return 1 ;;
    esac
}

download_fixed_archive() {
    local url="$1" filename="$2" expected="$3" output="$4" actual
    rm -f "$output"
    curl -sSLf --connect-timeout 10 --retry 1 --progress-bar --max-time 180 -o "$output" "$url" || { rm -f "$output"; return 1; }
    actual=$(openssl dgst -sha256 "$output" | awk '{print $NF}')
    [ "$actual" = "$expected" ] && return 0
    msg "⚠️ 下载包摘要不匹配: $filename" "⚠️ Downloaded archive checksum mismatch: $filename"
    rm -f "$output"
    return 1
}

swap_dependency_dir() {
    local stage="$1" target="$2" old="${2}.previous"
    rm -rf "$old"
    [ ! -e "$target" ] || mv "$target" "$old" || return 1
    if ! mv "$stage" "$target"; then
        [ ! -e "$old" ] || mv "$old" "$target"
        return 1
    fi
}

rollback_dependency_dir() {
    rm -rf "$1"
    [ ! -e "$2" ] || mv "$2" "$1"
}

install_uv_prebuilt() {
    local filename url expected archive_path stage scripts_dir
    if [ "$OS" = "darwin" ]; then
        if [ "$ARCH" = "arm64" ]; then filename="uv-0.13.7-py3-none-macosx_11_0_arm64.whl"; else filename="uv-0.13.7-py3-none-macosx_10_12_x86_64.whl"; fi
    elif [ "$ARCH" = "arm64" ]; then
        filename="uv-0.13.7-py3-none-manylinux_2_17_aarch64.manylinux2014_aarch64.musllinux_1_1_aarch64.whl"
    else
        filename="uv-0.13.7-py3-none-manylinux_2_17_x86_64.manylinux2014_x86_64.whl"
    fi
    expected=$(fixed_archive_sha256 "$filename") || return 1
    archive_path="$WORK_DIR/$filename"
    stage="$WORK_DIR/uv-stage"
    local ali_url="https://mirrors.aliyun.com/pypi/packages" official_url
    case "$filename" in
        *macosx_11_0_arm64*) ali_url+="/40/a5/8756c38ace102fa13fd4e653e8bc40e1f65924f1c0a0d23350339dac97e5/$filename" ;;
        *macosx_10_12_x86_64*) ali_url+="/3d/5b/e0379e7b172659420b1ec99a8e8229830aa85ea59684847c80b1c959a056/$filename" ;;
        *manylinux_2_17_aarch64*) ali_url+="/96/72/da1119f94034d7173e6c8e2cd77a6299744458f0af3cb8e3a04d5cd419d1/$filename" ;;
        *) ali_url+="/b9/93/0e053a2ca4523ee9825ccbedeb7121470d0a188933f11688b6d90d7f5c08/$filename" ;;
    esac
    official_url="https://files.pythonhosted.org/packages/${ali_url#https://mirrors.aliyun.com/pypi/packages/}"
    local urls=()
    if [ "$REGION" = cn ]; then urls=("$ali_url" "$official_url"); else urls=("$official_url" "$ali_url"); fi
    local fetched=false
    for url in "${urls[@]}"; do if download_fixed_archive "$url" "$filename" "$expected" "$archive_path"; then fetched=true; break; fi; done
    [ "$fetched" = true ] || return 1
    rm -rf "$stage"
    mkdir -p "$stage"
    if [ "$OS" = darwin ]; then ditto -x -k "$archive_path" "$stage" || { rm -rf "$stage" "$archive_path"; return 1; }
    else unzip -q "$archive_path" -d "$stage" || { rm -rf "$stage" "$archive_path"; return 1; }; fi
    scripts_dir=$(find "$stage" -type d -path '*/uv-0.13.7.data/scripts' -print -quit)
    [ -n "$scripts_dir" ] || { rm -rf "$stage" "$archive_path"; return 1; }
    mkdir -p "$stage/bin"
    cp "$scripts_dir/uv" "$stage/bin/uv" && cp "$scripts_dir/uvx" "$stage/bin/uvx" || { rm -rf "$stage" "$archive_path"; return 1; }
    chmod +x "$stage/bin/uv" "$stage/bin/uvx"
    local staged_uv_version
    staged_uv_version=""
    if [ -x "$stage/bin/uv" ]; then staged_uv_version=$(PATH="$stage/bin:$PATH" "$stage/bin/uv" --version 2>/dev/null || true); fi
    case "$staged_uv_version" in "uv $UV_VERSION"|"uv $UV_VERSION "*) ;; *) rm -rf "$stage" "$archive_path"; return 1 ;; esac
    if [ ! -x "$stage/bin/uv" ] || [ ! -x "$stage/bin/uvx" ] ||
       ! PATH="$stage/bin:$PATH" "$stage/bin/uvx" --version >/dev/null 2>&1; then
        rm -rf "$stage" "$archive_path"
        return 1
    fi
    local old="$DEPS_DIR/uv.previous"
    if ! swap_dependency_dir "$stage" "$DEPS_DIR/uv"; then rm -f "$archive_path"; return 1; fi
    ln -sf "$DEPS_DIR/uv/bin/uv" "$LOCAL_BIN/uv"
    ln -sf "$DEPS_DIR/uv/bin/uvx" "$LOCAL_BIN/uvx"
    hash -r
    if ! uv --version >/dev/null 2>&1 || ! uvx --version >/dev/null 2>&1; then
        rollback_dependency_dir "$DEPS_DIR/uv" "$old"
        rm -f "$archive_path"
        return 1
    fi
    rm -rf "$old" "$archive_path"
}

# 2. uv (先复用包管理器/PyPI，最后才取固定版本便携包)
if ! has_working_uv; then
    msg "   📦 正在安装 uv..." "   📦 Installing uv..."
    install_dependency_package uv uv "" || true
    if ! has_working_uv; then
        install_uv_prebuilt || msg "⚠️ uv 固定版本包安装失败，依赖检查将报告缺失。" "⚠️ Fixed uv archive installation failed; see dependency checks below."
    fi
fi

# 确保 uv / uvx 位于 ~/.local/bin 下（旧版 uv 可能装在 ~/.cargo/bin）
for uv_tool in uv uvx; do
    if [ ! -e "$LOCAL_BIN/$uv_tool" ] && [ -x "$HOME/.cargo/bin/$uv_tool" ]; then
        ln -sf "$HOME/.cargo/bin/$uv_tool" "$LOCAL_BIN/$uv_tool"
    fi
done

# 先本地查找 uv 已管理的解释器，查询不触发网络下载。
if ! has_working_python && has_working_uv; then
    UV_PYTHON_BIN=$(uv python find --managed-python 3.12 2>/dev/null || true)
    if [ -n "$UV_PYTHON_BIN" ] && [ -x "$UV_PYTHON_BIN" ]; then
        ln -sf "$UV_PYTHON_BIN" "$LOCAL_BIN/python3"
        ln -sf "$UV_PYTHON_BIN" "$LOCAL_BIN/python"
    fi
fi
if ! has_working_python; then install_dependency_package python@3.12 python312 python3 || true; fi

# 3. Python 3 (通过 uv 独立安装，无需编译器或 Xcode CLT)
if ! has_working_python && has_working_uv; then
    msg "   🐍 正在通过 uv 安装独立 Python 3 环境..." "   🐍 Installing standalone Python 3 via uv..."
    if [ -n "${UV_PYTHON_INSTALL_MIRROR:-}" ]; then
        uv python install 3.12 || true
    else
        local_python_mirror="https://registry.npmmirror.com/-/binary/python-build-standalone"
        local_python_proxy="https://ghproxy.net/https://github.com/astral-sh/python-build-standalone/releases/download"
        PYTHON_MIRRORS=()
        if [ "$REGION" = cn ]; then PYTHON_MIRRORS=("$local_python_mirror" "$local_python_proxy" "");
        else PYTHON_MIRRORS=("$local_python_proxy" "$local_python_mirror" ""); fi
        PYTHON_READY=false
        for python_mirror in "${PYTHON_MIRRORS[@]}"; do
            if [ -n "$python_mirror" ]; then
                if UV_PYTHON_INSTALL_MIRROR="$python_mirror" uv python install 3.12; then PYTHON_READY=true; break; fi
            elif env -u UV_PYTHON_INSTALL_MIRROR uv python install 3.12; then PYTHON_READY=true; break
            fi
        done
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

    local filename="node-v${NODE_VERSION}-${node_os}-${node_arch}.tar.gz"
    local expected tmp_node_archive="$WORK_DIR/node.tar.gz"
    expected=$(fixed_archive_sha256 "$filename") || return 1
    msg "   ⬇️  正在下载 Node.js (${filename})..." "   ⬇️  Downloading Node.js (${filename})..."
    local mirror_url="https://npmmirror.com/mirrors/node/v${NODE_VERSION}/$filename"
    local official_url="https://nodejs.org/dist/v${NODE_VERSION}/$filename" urls=() url fetched=false
    if [ "$REGION" = cn ]; then urls=("$mirror_url" "$official_url"); else urls=("$official_url" "$mirror_url"); fi
    if [ "$REGION" = auto ]; then
        local reachable=()
        for candidate in "$official_url" "$mirror_url"; do
            if curl -sSLf -I --connect-timeout 3 --max-time 5 "$candidate" >/dev/null 2>&1; then reachable+=("$candidate"); fi
        done
        if [ ${#reachable[@]} -gt 0 ]; then urls=("${reachable[@]}"); else urls=("$mirror_url" "$official_url"); fi
    fi
    for url in "${urls[@]}"; do if download_fixed_archive "$url" "$filename" "$expected" "$tmp_node_archive"; then fetched=true; break; fi; done
    [ "$fetched" = true ] || return 1

    local stage="$WORK_DIR/node-stage" old="$DEPS_DIR/node.previous"
    rm -rf "$stage" "$old"
    mkdir -p "$stage"
    local staged_node_version
    staged_node_version=""
    tar -xzf "$tmp_node_archive" -C "$stage" --strip-components=1 || { rm -rf "$stage" "$tmp_node_archive"; return 1; }
    if [ -x "$stage/bin/node" ]; then staged_node_version=$(PATH="$stage/bin:$PATH" "$stage/bin/node" --version 2>/dev/null || true); fi
    if [ ! -x "$stage/bin/node" ] || [ ! -x "$stage/bin/npm" ] || [ ! -x "$stage/bin/npx" ] ||
       [ "$staged_node_version" != "v${NODE_VERSION}" ] ||
       ! PATH="$stage/bin:$PATH" "$stage/bin/npm" --version >/dev/null 2>&1 ||
       ! PATH="$stage/bin:$PATH" "$stage/bin/npx" --version >/dev/null 2>&1; then
        rm -rf "$stage" "$tmp_node_archive"
        return 1
    fi
    if ! swap_dependency_dir "$stage" "$DEPS_DIR/node"; then rm -f "$tmp_node_archive"; return 1; fi
    for bin_item in node npm npx; do ln -sf "$DEPS_DIR/node/bin/$bin_item" "$LOCAL_BIN/$bin_item"; done
    hash -r
    if ! has_working_node; then
        rollback_dependency_dir "$DEPS_DIR/node" "$old"
        rm -f "$tmp_node_archive"
        return 1
    fi
    rm -rf "$old"
    rm -f "$tmp_node_archive"
    return 0
}

if ! has_working_node; then
    msg "   🟢 正在安装 Node.js v${NODE_VERSION}..." "   🟢 Installing Node.js v${NODE_VERSION}..."
    if ! install_node_prebuilt; then
        msg "⚠️ Node.js 预编译包安装失败，请检查网络或系统兼容性。" \
            "⚠️ Node.js archive installation failed. Check network and OS compatibility."
    fi
fi

has_working_ffmpeg() {
    command -v ffmpeg >/dev/null 2>&1 && ffmpeg -version >/dev/null 2>&1 &&
        command -v ffprobe >/dev/null 2>&1 && ffprobe -version >/dev/null 2>&1
}
if ! has_working_ffmpeg; then
    install_dependency_package ffmpeg ffmpeg ffmpeg || true
fi
if ! has_working_ffmpeg; then
    msg "⚠️ FFmpeg/ffprobe 未就绪，媒体处理可能不可用。" "⚠️ FFmpeg/ffprobe are unavailable; media processing may be unavailable."
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
fi

configure_language_mirrors() {
    [ "${POLARIS_CONFIGURE_MIRRORS:-0}" = 1 ] || return 0
    if has_working_node && [ -z "${NPM_CONFIG_REGISTRY:-}" ] && [ -z "${NPM_CONFIG_USERCONFIG:-}" ] &&
       [ -z "${npm_config_registry:-}" ] && [ -z "${npm_config_userconfig:-}" ]; then
        local registry
        registry=$(npm config get registry 2>/dev/null || true)
        if [ -z "$registry" ] || [ "$registry" = "https://registry.npmjs.org" ] || [ "$registry" = "https://registry.npmjs.org/" ]; then
            npm config set registry https://registry.npmmirror.com >/dev/null 2>&1 || msg "⚠️ npm 用户源配置失败。" "⚠️ Failed to configure the user npm registry."
        fi
    fi
    if has_working_python && [ -z "${PIP_INDEX_URL:-}" ] && [ -z "${PIP_CONFIG_FILE:-}" ]; then
        local pip_index
        pip_index=$(python3 -m pip config get global.index-url 2>/dev/null || true)
        if [ -z "$pip_index" ] || [ "$pip_index" = "https://pypi.org/simple" ] || [ "$pip_index" = "https://pypi.org/simple/" ]; then
            python3 -m pip config --user set global.index-url https://mirrors.aliyun.com/pypi/simple/ >/dev/null 2>&1 || msg "⚠️ pip 用户源配置失败。" "⚠️ Failed to configure the user pip index."
        fi
    fi
    local uv_config="${XDG_CONFIG_HOME:-$HOME/.config}/uv/uv.toml"
    if [ ! -e "$uv_config" ] && [ -z "${UV_CONFIG_FILE:-}" ] && [ -z "${UV_DEFAULT_INDEX:-}" ] &&
       [ -z "${UV_INDEX_URL:-}" ] && [ -z "${UV_PYTHON_INSTALL_MIRROR:-}" ]; then
        if ! mkdir -p "${uv_config%/*}" || ! printf 'python-install-mirror = "https://registry.npmmirror.com/-/binary/python-build-standalone"\n[[index]]\nurl = "https://mirrors.aliyun.com/pypi/simple/"\ndefault = true\n' > "$uv_config"; then
            msg "⚠️ uv 用户源配置失败。" "⚠️ Failed to configure the user uv index."
        fi
    fi
}
configure_language_mirrors

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
    msg "✅ 核心运行环境已就绪。" "✅ Core runtime dependencies are ready."
fi
if [ "$OS" = linux ] && ! command -v bwrap >/dev/null 2>&1; then
    DEPS_READY=false
    msg "⚠️ 缺少 bubblewrap，隔离执行不可用，请通过系统包管理器安装。" \
        "⚠️ bubblewrap is missing; isolated execution is unavailable. Install it with your package manager."
fi
if [ "$DEPS_READY" != true ]; then
    msg "❌ 核心依赖未就绪，未替换现有 Polaris 安装。请补齐依赖后重试。" \
        "❌ Core dependencies are not ready; the existing Polaris installation was preserved. Install missing dependencies and retry."
    exit 1
fi

# ── 2. 确定 Polaris 下载源 ─────────────────────────────────────────────────
ARCHIVE_NAME="${BIN_NAME}-${OS}-${ARCH}.tar.gz"
GITHUB_BASE="https://github.com/${REPO}/releases/latest/download"
DIRECT_URL="${GITHUB_BASE}/${ARCHIVE_NAME}"

CANDIDATE_URLS=()
if [ -n "${POLARIS_DOWNLOAD_BASE:-}" ]; then
    CANDIDATE_URLS+=("${POLARIS_DOWNLOAD_BASE%/}/${ARCHIVE_NAME}")
fi
if [ "$REGION" = cn ]; then
    CANDIDATE_URLS+=("https://ghproxy.net/${DIRECT_URL}" "$DIRECT_URL" "https://mirror.ghproxy.com/${DIRECT_URL}")
else
    CANDIDATE_URLS+=("$DIRECT_URL" "https://ghproxy.net/${DIRECT_URL}" "https://mirror.ghproxy.com/${DIRECT_URL}")
fi
if [ "$REGION" = auto ]; then
    PROBED_URLS=() UNPROBED_URLS=()
    for candidate in "${CANDIDATE_URLS[@]}"; do
        if curl -sSLf --connect-timeout 3 --max-time 6 -o /dev/null "${candidate}.sha256" 2>/dev/null; then PROBED_URLS+=("$candidate"); else UNPROBED_URLS+=("$candidate"); fi
    done
    CANDIDATE_URLS=()
    if [ -n "${POLARIS_DOWNLOAD_BASE:-}" ]; then CANDIDATE_URLS+=("${POLARIS_DOWNLOAD_BASE%/}/${ARCHIVE_NAME}"); fi
    for candidate in "${PROBED_URLS[@]}"; do
        [ -n "${POLARIS_DOWNLOAD_BASE:-}" ] && [ "$candidate" = "${POLARIS_DOWNLOAD_BASE%/}/${ARCHIVE_NAME}" ] && continue
        CANDIDATE_URLS+=("$candidate")
    done
    if [ ${#PROBED_URLS[@]} -eq 0 ]; then
        CANDIDATE_URLS=()
        [ -n "${POLARIS_DOWNLOAD_BASE:-}" ] && CANDIDATE_URLS+=("${POLARIS_DOWNLOAD_BASE%/}/${ARCHIVE_NAME}")
        CANDIDATE_URLS+=("https://ghproxy.net/${DIRECT_URL}" "https://mirror.ghproxy.com/${DIRECT_URL}" "$DIRECT_URL")
    else
        [ ${#UNPROBED_URLS[@]} -eq 0 ] || CANDIDATE_URLS+=("${UNPROBED_URLS[@]}")
    fi
fi

if [ ${#CANDIDATE_URLS[@]} -eq 0 ]; then
    msg "❌ 无可用下载源。" "❌ No download sources available."
    exit 1
fi

# ── 2. 下载、校验并按完整发布三件套切换源 ───────────────────────────────────
TMP_ARCHIVE="$WORK_DIR/polaris.tar.gz"
TMP_SHA_FILE="${TMP_ARCHIVE}.sha256"
TMP_SIG_B64="$WORK_DIR/release.sig.b64"
TMP_SIG="$WORK_DIR/release.sig"
TMP_DIR="$WORK_DIR/extracted"
msg "⬇️  开始下载 Polaris（校验失败后尝试下一源）..." "⬇️  Downloading Polaris (retrying sources after verification failures)..."

# RELEASE_PUBLIC_KEYS_BEGIN
RELEASE_PUBLIC_KEYS=(
    'MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAERq8RvYaXY9cy0aXOFTmU6sNNzIRikjdQoUZuy/iZ0HIAaZNDMmAYhxgqAdpIdUfiAZUZOCUxL/9ghaSAHZs3Bw=='
)
# RELEASE_PUBLIC_KEYS_END
DOWNLOADED=false
for URL in "${CANDIDATE_URLS[@]}"; do
    msg "   尝试: $URL" "   Trying: $URL"
    rm -f "$TMP_ARCHIVE" "$TMP_SHA_FILE" "$TMP_SIG_B64" "$TMP_SIG"
    if ! curl -sSLf --connect-timeout 10 --retry 1 --progress-bar --max-time 300 -o "$TMP_ARCHIVE" "$URL" ||
       ! curl -sSLf --connect-timeout 10 --max-time 30 -o "$TMP_SHA_FILE" "${URL}.sha256" ||
       ! curl -sSLf --connect-timeout 10 --max-time 30 -o "$TMP_SIG_B64" "${URL}.sha256.sig" ||
       ! openssl base64 -d -A -in "$TMP_SIG_B64" -out "$TMP_SIG"; then
        msg "   发布文件或校验文件不可达，尝试下一源。" "   Artifact or checksum file unavailable; trying next source."
        continue
    fi
    SIGNATURE_VALID=false
    for release_key in "${RELEASE_PUBLIC_KEYS[@]}"; do
        printf '%s' "$release_key" | openssl base64 -d -A -out "$WORK_DIR/release-key.der" 2>/dev/null || continue
        if openssl pkey -pubin -inform DER -in "$WORK_DIR/release-key.der" -out "$WORK_DIR/release-key.pem" 2>/dev/null &&
           openssl dgst -sha256 -verify "$WORK_DIR/release-key.pem" -signature "$TMP_SIG" "$TMP_SHA_FILE" >/dev/null 2>&1; then SIGNATURE_VALID=true; break; fi
    done
    EXPECTED_SHA=$(awk '{print $1}' "$TMP_SHA_FILE")
    ACTUAL_SHA=$(openssl dgst -sha256 "$TMP_ARCHIVE" | awk '{print $NF}')
    if [ "$SIGNATURE_VALID" != true ] || [ -z "$EXPECTED_SHA" ] || [ "$EXPECTED_SHA" != "$ACTUAL_SHA" ]; then
        msg "   发布签名或摘要无效，拒绝此源并继续尝试。" "   Invalid release signature or checksum; rejecting source and trying next."
        continue
    fi
    DOWNLOADED=true
    break
done
if [ "$DOWNLOADED" != true ]; then
    msg "❌ 所有下载源均未提供有效发布三件套。" "❌ No source provided a valid signed release set."
    exit 1
fi
rm -f "$TMP_SHA_FILE" "$TMP_SIG_B64" "$TMP_SIG"
msg "✅ 发布签名与摘要校验通过。" "✅ Release signature and checksum verified."

# ── 3. 校验 & 解压 ────────────────────────────────────────────────────────────
msg "📦 正在解压..." "📦 Extracting..."

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
            health_status=$(curl -sS --noproxy '*' --max-time 2 -o /dev/null -w '%{http_code}' "${CONSOLE_URL%/}/healthz" 2>/dev/null || true)
            home_status=$(curl -sS --noproxy '*' --max-time 2 -o /dev/null -w '%{http_code}' "${CONSOLE_URL%/}/" 2>/dev/null || true)
            if [ "$health_status" = 200 ] && [ "$home_status" = 200 ]; then
                SERVICE_READY=true
                break
            fi
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
