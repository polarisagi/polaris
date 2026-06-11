#!/usr/bin/env bash
# 重新编译并重启 Polaris（前端 + 后端）
# 用法：
#   ./scripts/restart.sh          # 构建前端 + Go，重启（复用已有 Rust dylib）
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

PORT=28888
DATA_DIR="${POLARIS_DATA_DIR:-$HOME/.polarisagi/polaris}"
mkdir -p "$DATA_DIR"
LOG_FILE="$DATA_DIR/logs/polaris.log"
LOG_MAX_BYTES=10485760  # 10 MB，超过则截断

# ── 平台检测 ─────────────────────────────────────────────
case "$(uname -s)" in
  Darwin) DYLIB="libsubstrate.dylib" ;;
  Linux)  DYLIB="libsubstrate.so" ;;
  MINGW*|MSYS*|CYGWIN*) DYLIB="substrate.dll" ;;
  *) echo "✗ 不支持的平台：$(uname -s)"; exit 1 ;;
esac
DYLIB_SRC="rust/substrate/target/release/$DYLIB"
DYLIB_DST="bin/lib/$DYLIB"

# ── 0. 日志截断 ───────────────────────────────────────────
if [[ -f "$LOG_FILE" ]]; then
  size=$(wc -c < "$LOG_FILE" 2>/dev/null || echo 0)
  if (( size > LOG_MAX_BYTES )); then
    echo "→ 日志超过 10MB，截断..."
    tail -c 2097152 "$LOG_FILE" > "${LOG_FILE}.tmp" && mv "${LOG_FILE}.tmp" "$LOG_FILE"
  fi
fi

# ── 1. 停止旧进程（仅杀 :PORT 上的进程）──────────────────
echo "→ 停止旧进程..."
# lsof -ti 可能返回多行 PID，必须逐行处理
OLD_PIDS=$(lsof -ti:"$PORT" 2>/dev/null || true)
if [[ -n "$OLD_PIDS" ]]; then
  while IFS= read -r pid; do
    [[ -z "$pid" ]] && continue
    kill "$pid" 2>/dev/null || true
  done <<< "$OLD_PIDS"

  # 等待所有旧进程退出（最多 5s），超时逐个 kill -9
  for i in {1..5}; do
    sleep 1
    STILL_ALIVE=$(lsof -ti:"$PORT" 2>/dev/null || true)
    if [[ -z "$STILL_ALIVE" ]]; then
      echo "  旧进程已全部退出"
      break
    fi
    if [[ $i == 5 ]]; then
      echo "  优雅退出超时，强制终止..."
      while IFS= read -r pid; do
        [[ -z "$pid" ]] && continue
        kill -9 "$pid" 2>/dev/null || true
      done <<< "$STILL_ALIVE"
      sleep 0.5
    fi
  done
fi

# 确认端口已释放
if lsof -ti:"$PORT" &>/dev/null; then
  echo "✗ 端口 $PORT 仍被占用，无法启动"
  exit 1
fi

# ── 2. Rust FFI（--full 时重建；否则验证 dylib 存在）──────
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

# ── 3. 前端 ───────────────────────────────────────────────
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



# ── 4. 复制 dylib 并构建 Go 后端 ─────────────────────────
echo "→ 构建 Go 后端..."
mkdir -p bin/lib
cp "$DYLIB_SRC" "$DYLIB_DST"
CGO_ENABLED=0 go build -o bin/polaris ./cmd/polaris

# ── 5. 安装到标准目录 ─────────────────────────────────────
INSTALL_DIR="$HOME/.polarisagi/polaris/bin"
echo "→ 复制执行文件到标准目录 $INSTALL_DIR ..."
mkdir -p "$INSTALL_DIR/lib"
cp bin/polaris "$INSTALL_DIR/"
cp -r bin/lib/* "$INSTALL_DIR/lib/"

# ── 6. 启动 ───────────────────────────────────────────────
echo "→ 启动 Polaris..."
# Polaris 遵循配置层规范，默认使用 ~/.polarisagi/polaris/config.toml
mkdir -p "$(dirname "$LOG_FILE")"
nohup "$INSTALL_DIR/polaris" >> "$LOG_FILE" 2>&1 &

# 等待最多 5s 确认端口监听
for i in {1..10}; do
  sleep 0.5
  NEW_PID=$(lsof -ti:"$PORT" 2>/dev/null || true)
  if [[ -n "$NEW_PID" ]]; then
    echo "✓ Polaris 已启动  PID=${NEW_PID}  http://localhost:${PORT}"
    exit 0
  fi
done

echo "✗ 启动失败，最近日志："
tail -30 "$LOG_FILE"
exit 1
