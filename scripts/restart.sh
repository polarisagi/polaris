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
# 用法：
#   ./scripts/restart.sh          # 构建前端 + Go，热部署并重启常驻服务
#   ./scripts/restart.sh --full   # 同上 + 重新构建 Rust FFI（Rust 代码有变更时使用）

set -euo pipefail

PROJECT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$PROJECT_ROOT"

FULL_BUILD=false
for arg in "$@"; do
  if [[ "$arg" == "--full" ]]; then
    FULL_BUILD=true
  fi
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

if $READY; then
  echo "✓ Polaris 已部署并启动  http://localhost:${PORT}"
  rm -f "$PREV_BIN" 2>/dev/null || true
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
