#!/usr/bin/env bash
# 重新编译 Polaris（前端 + 后端）并热部署到「正常安装目录」，替换常驻服务
# （launchd LaunchAgent / systemd --user，label/unit 见 cmd/polaris/cli_service.go
# serviceLabel）实际运行的那份二进制，用最新代码测试。
#
# 2026-09-22 由「独立 .devdata 沙箱端口」模式改为直接部署安装目录：桌面外壳
# （desktop/，ADR-0096）与所有真实使用场景连的都是这个常驻服务，不是某个隔离
# 测试端口——同一天的 empty_response 排查中，故障只在这条真实路径上复现，
# 隔离沙箱测不出桌面外壳触发的问题。数据目录与真实使用共享，不再是一次性
# 可丢弃的开发库；风险由本脚本的自动回滚兜底（新版本起不来则自动换回上一份
# 二进制），不做数据库快照——DB 迁移风险仍在，改表前仍需遵守
# CLAUDE.md「DDL 修改策略」。
#
# 2026-09-24 追加桌面外壳（desktop/，ADR-0096）打包：外壳与守护进程同批构建、
# 同批替换，避免新守护进程配旧外壳（或反之）导致只在真实路径上出现的契约漂移
# （外壳经 `polaris service status --json` 取端口/令牌，字段两侧须同版本）。
# 外壳只替换已安装位置（macOS：/Applications/Polaris.app），替换前若在运行则
# 先退出、替换后重新拉起；它自己经 service status 重新发现新端口，脚本不写端口。
#
# 用法：
#   ./scripts/restart.sh               # 构建前端 + Go + 桌面外壳，热部署并重启
#   ./scripts/restart.sh --full        # 同上 + 重新构建 Rust FFI（Rust 代码有变更时使用）
#   ./scripts/restart.sh --no-desktop  # 跳过桌面外壳（只改了内核时省去数分钟 LTO 构建）

set -euo pipefail

PROJECT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$PROJECT_ROOT"

FULL_BUILD=false
BUILD_DESKTOP=true
for arg in "$@"; do
  case "$arg" in
    --full)       FULL_BUILD=true ;;
    --no-desktop) BUILD_DESKTOP=false ;;
    *) echo "✗ 未知参数：$arg（可用：--full / --no-desktop）"; exit 1 ;;
  esac
done

# ── 安装目录布局（与 scripts/install.sh / cli_service.go serviceLabel 一致）──
INSTALL_DIR="$HOME/.polarisagi/polaris"
BIN_DIR="$INSTALL_DIR/bin"
LOG_OUT="$INSTALL_DIR/logs/service.out.log"
LOG_ERR="$INSTALL_DIR/logs/service.err.log"
PORT_FILE="$INSTALL_DIR/run/polaris.port"
SERVICE_LABEL="com.polarisagi.polaris"

OS="$(uname -s)"
case "$OS" in
  Darwin) DYLIB="libsubstrate.dylib" ;;
  Linux)  DYLIB="libsubstrate.so" ;;
  *) echo "✗ 本脚本的服务热部署仅支持 macOS / Linux（当前：$OS）。Windows 请用 scripts/install.ps1 + polaris service。"; exit 1 ;;
esac
DYLIB_SRC="rust/substrate/target/release/$DYLIB"

# 桌面外壳安装位置与产物（bundle 目标见 desktop/src-tauri/tauri.conf.json）
DESKTOP_APP_NAME="Polaris"
case "$OS" in
  Darwin)
    DESKTOP_BUNDLE="app"
    DESKTOP_ARTIFACT="desktop/src-tauri/target/release/bundle/macos/$DESKTOP_APP_NAME.app"
    DESKTOP_INSTALL="/Applications/$DESKTOP_APP_NAME.app"
    ;;
  Linux)
    DESKTOP_BUNDLE="appimage"
    DESKTOP_ARTIFACT=""   # AppImage 文件名带版本与架构，构建后按通配定位
    DESKTOP_INSTALL="$HOME/.local/bin/polaris-desktop.AppImage"
    ;;
esac

# ── 1. Rust FFI（--full 时重建；否则验证 dylib 存在）──────
if $FULL_BUILD; then
  echo "→ 构建 Rust FFI（--full 模式，约 60~120s）..."
  # CFLAGS= LDFLAGS= 防止 Go/shell 链接标志污染 aws-lc-sys 的 C 编译环境
  CFLAGS= LDFLAGS= cargo build --release --manifest-path rust/substrate/Cargo.toml
else
  if [[ ! -f "$DYLIB_SRC" ]]; then
    echo "✗ Rust dylib 不存在：$DYLIB_SRC"
    echo "  首次使用或 Rust 代码有变更，请运行：./scripts/restart.sh --full"
    exit 1
  fi
  echo "→ 复用已有 Rust dylib（如需重建请加 --full）"
fi

# ── 2. 前端 ───────────────────────────────────────────────
echo "→ 构建前端 (web/)..."
cd web
# 仅当 package.json / package-lock.json 比 node_modules 新时才 install
if [[ ! -d node_modules ]] || \
   [[ package.json -nt node_modules/.package-lock.json ]] || \
   [[ package-lock.json -nt node_modules/.package-lock.json ]]; then
  echo "  npm install..."
  npm install --silent --no-fund --no-audit
else
  echo "  node_modules 已是最新，跳过 npm install"
fi
npm run build
cd ..

# ── 3. 构建 Go 后端（先落到源码目录 bin/，构建失败绝不触碰安装目录）──
echo "→ 构建 Go 后端..."
mkdir -p bin/lib
cp "$DYLIB_SRC" "bin/lib/$DYLIB"
CGO_ENABLED=0 go build -o bin/polaris ./cmd/polaris

# ── 3b. 桌面外壳（同样先构建、后替换：构建失败不停服务、不动已安装外壳）──
if $BUILD_DESKTOP; then
  echo "→ 构建桌面外壳 (desktop/, bundle=$DESKTOP_BUNDLE，首次约数分钟)..."
  if ! cargo tauri --version &>/dev/null; then
    echo "  未安装 tauri-cli，安装中（一次性，与 release.yml desktop job 同版本约束）..."
    cargo install tauri-cli --version "^2" --locked
  fi
  (cd desktop/src-tauri && CFLAGS= LDFLAGS= cargo tauri build --bundles "$DESKTOP_BUNDLE")
  if [[ "$OS" == "Linux" ]]; then
    DESKTOP_ARTIFACT=$(ls -t desktop/src-tauri/target/release/bundle/appimage/*.AppImage 2>/dev/null | head -1 || true)
  fi
  if [[ -z "$DESKTOP_ARTIFACT" || ! -e "$DESKTOP_ARTIFACT" ]]; then
    echo "✗ 未找到桌面外壳构建产物：${DESKTOP_ARTIFACT:-bundle/appimage/*.AppImage}"
    exit 1
  fi
fi

# ── 4. 停止常驻服务 ───────────────────────────────────────
# 直接走系统服务管理器停止，不依赖安装目录里现存二进制是否还能正常执行
# （若上一轮部署留下了个跑不起来的版本，仍要能停干净）。
echo "→ 停止常驻服务 ($SERVICE_LABEL)..."
case "$OS" in
  Darwin)
    launchctl bootout "gui/$(id -u)" "$HOME/Library/LaunchAgents/$SERVICE_LABEL.plist" 2>/dev/null || true
    ;;
  Linux)
    systemctl --user stop polaris.service 2>/dev/null || true
    ;;
esac
# 兜底：服务未注册但有手工启动的旧进程（历史遗留场景），或 KeepAlive 竞态下
# 短暂复活的残留进程。
for i in {1..10}; do
  PIDS=$(pgrep -f "$BIN_DIR/polaris serve" 2>/dev/null || true)
  [[ -z "$PIDS" ]] && break
  kill $PIDS 2>/dev/null || true
  sleep 1
  if [[ $i -eq 10 ]]; then
    kill -9 $PIDS 2>/dev/null || true
  fi
done

# ── 5. 部署：替换安装目录下的二进制（保留一份可回滚的上一版本）──
echo "→ 部署到安装目录 ($BIN_DIR)..."
mkdir -p "$BIN_DIR/lib"
PREV_BIN=""
if [[ -f "$BIN_DIR/polaris" ]]; then
  PREV_BIN="$BIN_DIR/polaris.prev"
  cp "$BIN_DIR/polaris" "$PREV_BIN"
fi
cp bin/polaris "$BIN_DIR/polaris"
chmod +x "$BIN_DIR/polaris"
cp "bin/lib/$DYLIB" "$BIN_DIR/lib/$DYLIB"

# ── 6. 启动常驻服务（复用 `polaris service install`：写单元文件 + unload/load，
#      与 scripts/install.sh 首次安装走同一条路径，不另造一套注册逻辑）──
echo "→ 启动常驻服务..."
"$BIN_DIR/polaris" service install >/dev/null

READY=false
PORT=""
for i in {1..20}; do
  sleep 0.5
  if [[ -f "$PORT_FILE" ]]; then
    PORT=$(cat "$PORT_FILE" 2>/dev/null || true)
    if [[ -n "$PORT" ]] && lsof -ti:"$PORT" &>/dev/null; then
      READY=true
      break
    fi
  fi
done

# 桌面外壳：放在守护进程确认就绪之后——外壳启动即经 service status 连接，
# 守护进程起不来时换上新外壳只会多一个报错窗口；且回滚只覆盖守护进程二进制。
#
# 即使 --no-desktop 不替换外壳，仍在运行的外壳也要重启：守护进程每次启动都轮换
# run/polaris.token，旧窗口持旧 Cookie 轮询 /v1 会连续 401，触发按 IP 的鉴权冷却，
# 连带把同在 127.0.0.1 上的 CLI 与新窗口一起锁 5 分钟（2026-09-25 实测）。
DESKTOP_PROC_PATTERN="$DESKTOP_INSTALL"
[[ "$OS" == "Darwin" ]] && DESKTOP_PROC_PATTERN="$DESKTOP_INSTALL/Contents/MacOS/"

stop_desktop() {
  pgrep -f "$DESKTOP_PROC_PATTERN" &>/dev/null || return 1
  [[ "$OS" == "Darwin" ]] && osascript -e "quit app \"$DESKTOP_APP_NAME\"" 2>/dev/null
  for i in {1..10}; do
    pgrep -f "$DESKTOP_PROC_PATTERN" &>/dev/null || return 0
    [[ $i -ge 4 ]] && pkill -f "$DESKTOP_PROC_PATTERN" 2>/dev/null
    sleep 0.5
  done
  return 0
}

start_desktop() {
  case "$OS" in
    Darwin) open "$DESKTOP_INSTALL" ;;
    Linux)  (nohup "$DESKTOP_INSTALL" >/dev/null 2>&1 &) ;;
  esac
}

install_desktop() {
  echo "→ 替换桌面外壳 ($DESKTOP_INSTALL)..."
  case "$OS" in
    Darwin)
      # 整包替换而非覆盖拷贝：旧包里已删除的资源不得残留在新包中
      rm -rf "$DESKTOP_INSTALL"
      ditto "$DESKTOP_ARTIFACT" "$DESKTOP_INSTALL"
      # 本地构建无证书，tauri 产出的包只有链接器给可执行文件的 ad-hoc 签名，
      # 包级签名校验不过（"code has no resources"）；补一次包级 ad-hoc 签名，
      # 否则系统对其通知/钥匙串等按身份授权的能力会随每次替换失效或拒绝。
      codesign --force --deep --sign - "$DESKTOP_INSTALL" >/dev/null 2>&1 || true
      ;;
    Linux)
      mkdir -p "$(dirname "$DESKTOP_INSTALL")"
      cp "$DESKTOP_ARTIFACT" "$DESKTOP_INSTALL"
      chmod +x "$DESKTOP_INSTALL"
      ;;
  esac
}

deploy_desktop() {
  local was_running=false
  stop_desktop && was_running=true
  if $BUILD_DESKTOP; then
    install_desktop
  fi
  if $was_running; then
    start_desktop
    echo "✓ 桌面外壳已重新拉起（附着新令牌）"
  elif $BUILD_DESKTOP; then
    echo "✓ 桌面外壳已替换"
  fi
}

if $READY; then
  echo "✓ Polaris 已部署并启动  http://localhost:${PORT}"
  rm -f "$PREV_BIN" 2>/dev/null || true
  deploy_desktop
  exit 0
fi

echo "✗ 新版本启动失败，最近日志："
tail -30 "$LOG_ERR" 2>/dev/null || true
tail -30 "$LOG_OUT" 2>/dev/null || true

if [[ -n "$PREV_BIN" ]]; then
  echo "→ 自动回滚到上一版本..."
  case "$OS" in
    Darwin) launchctl bootout "gui/$(id -u)" "$HOME/Library/LaunchAgents/$SERVICE_LABEL.plist" 2>/dev/null || true ;;
    Linux)  systemctl --user stop polaris.service 2>/dev/null || true ;;
  esac
  cp "$PREV_BIN" "$BIN_DIR/polaris"
  "$BIN_DIR/polaris" service install >/dev/null 2>&1 || true
  echo "✓ 已回滚，旧版本继续提供服务"
fi
exit 1
