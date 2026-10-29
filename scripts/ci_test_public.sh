#!/usr/bin/env bash
# 公开项目的本地发布门控；与 .github/workflows/ci.yml 使用相同检查入口。
set -u
set -o pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$ROOT" || exit 2

STAMP=$(date +%Y%m%d-%H%M%S)
LOG_DIR=${CI_LOG_DIR:-"$ROOT/.ci-logs/public-$STAMP"}
mkdir -p "$LOG_DIR" || exit 2
TMP_DIR=$(mktemp -d "${TMPDIR:-/tmp}/polaris-public-ci.XXXXXX") || exit 2
trap 'rm -rf "$TMP_DIR"' EXIT

STEP_NAMES=()
STEP_STATUS=()
STEP_SECONDS=()
STEP_LOGS=()
FAILED=0

run_step() {
  local name=$1
  shift
  local index=${#STEP_NAMES[@]}
  local log_file
  local started=$SECONDS
  local result=0
  printf -v log_file '%s/%02d.log' "$LOG_DIR" "$((index + 1))"

  STEP_NAMES+=("$name")
  STEP_LOGS+=("$log_file")
  printf '\n▶ %s\n$' "$name"
  printf ' %q' "$@"
  printf '\n'

  "$@" >"$log_file" 2>&1 || result=$?
  STEP_SECONDS+=("$((SECONDS - started))")
  if (( result == 0 )); then
    STEP_STATUS+=(PASS)
    tail -n 12 "$log_file"
    printf '✅ PASS (%ss)\n' "$((SECONDS - started))"
  else
    STEP_STATUS+=(FAIL)
    FAILED=1
    tail -n 80 "$log_file"
    printf '❌ FAIL (exit %s, %ss) — 完整日志: %s\n' "$result" "$((SECONDS - started))" "$log_file"
  fi
}

check_tools() {
  local missing=0
  local name
  for name in go cargo rustc node npm git tar make; do
    if ! command -v "$name" >/dev/null 2>&1; then
      printf '缺少必要命令: %s\n' "$name" >&2
      missing=1
    fi
  done
  if ! command -v cargo-deny >/dev/null 2>&1; then
    printf '缺少必要命令: cargo-deny（安装：cargo install cargo-deny --locked）\n' >&2
    missing=1
  fi
  if ! command -v shasum >/dev/null 2>&1 && ! command -v sha256sum >/dev/null 2>&1; then
    printf '缺少 SHA-256 工具: shasum 或 sha256sum\n' >&2
    missing=1
  fi
  return "$missing"
}

check_go_format() {
  local -a files
  local file bad
  files=()
  while IFS= read -r -d '' file; do files+=("$file"); done < <(git ls-files -z -- '*.go')
  [ "${#files[@]}" -eq 0 ] && return 0
  bad=$(gofmt -l "${files[@]}") || return
  if [ -n "$bad" ]; then
    printf '以下 Go 文件未格式化:\n%s\n' "$bad" >&2
    return 1
  fi
}

check_manifest_clean() {
  git diff --exit-code -- internal/config/kernel_manifest.json
}

check_diff_clean() {
  git diff --check
  git diff --cached --check
}

check_build_succeeded() {
  local i
  for i in "${!STEP_NAMES[@]}"; do
    if [ "${STEP_NAMES[$i]}" = '完整源码构建（Go + Rust + Web）' ]; then
      [ "${STEP_STATUS[$i]}" = PASS ] && return 0
      printf '完整源码构建未通过，跳过依赖其产物的安装包验证。\n' >&2
      return 1
    fi
  done
  printf '没有执行完整源码构建，不能验证安装包。\n' >&2
  return 1
}

cross_compile() {
  local target
  for target in linux/amd64 windows/amd64 darwin/arm64; do
    local goos=${target%/*}
    local goarch=${target#*/}
    printf '交叉编译 %s/%s\n' "$goos" "$goarch"
    CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" go build -o "$TMP_DIR/polaris-$goos-$goarch" ./cmd/polaris || return
  done
}

package_local() {
  local os arch archive stage
  check_build_succeeded || return
  [ -x bin/polaris ] || { printf '缺少可执行文件 bin/polaris\n' >&2; return 1; }
  [ -d configs ] || { printf '缺少 configs/ 配置目录\n' >&2; return 1; }
  [ -f LICENSE ] || { printf '缺少 LICENSE\n' >&2; return 1; }
  os=$(uname -s | tr '[:upper:]' '[:lower:]')
  arch=$(uname -m)
  case "$arch" in
    x86_64) arch=amd64 ;;
    aarch64|arm64) arch=arm64 ;;
  esac
  archive="$TMP_DIR/polaris-$os-$arch.tar.gz"
  stage="$TMP_DIR/package/polaris"
  mkdir -p "$stage"
  cp bin/polaris "$stage/"
  cp -R configs "$stage/"
  cp LICENSE "$stage/"
  if [ -d bin/lib ]; then cp -R bin/lib "$stage/"; fi
  tar -czf "$archive" -C "$TMP_DIR/package" polaris || return
  tar -tzf "$archive" > "$TMP_DIR/archive.list" || return
  grep -Fxq 'polaris/polaris' "$TMP_DIR/archive.list" || { printf '安装包缺少 polaris/polaris\n' >&2; return 1; }
  grep -Fxq 'polaris/LICENSE' "$TMP_DIR/archive.list" || { printf '安装包缺少许可证\n' >&2; return 1; }
  grep -Fq 'polaris/configs/' "$TMP_DIR/archive.list" || { printf '安装包缺少配置文件\n' >&2; return 1; }
  if command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$archive" > "$archive.sha256" || return
    shasum -a 256 -c "$archive.sha256" || return
  else
    sha256sum "$archive" > "$archive.sha256" || return
    sha256sum -c "$archive.sha256" || return
  fi
  printf '已生成并校验本机安装包: %s\n' "$archive"
}

printf 'Polaris public release checks\n日志目录: %s\n' "$LOG_DIR"
run_step '检查构建工具链' check_tools
run_step '检查 Git 差异空白错误' check_diff_clean
run_step '安装锁定的前端依赖' npm ci --prefix web
run_step '前端依赖漏洞审计' npm audit --prefix web --audit-level=moderate
run_step 'Go 源码格式' check_go_format
# unsafe.Pointer 是 Rust/C FFI 的预期边界；保留其他 vet 检查。
run_step 'Go 静态分析（排除 FFI unsafe.Pointer 误报）' go vet -unsafeptr=false ./...
run_step 'Rust 格式检查' cargo fmt --manifest-path rust/substrate/Cargo.toml --check
run_step 'Rust Clippy' cargo clippy --locked --manifest-path rust/substrate/Cargo.toml --all-targets -- -D warnings
run_step 'Rust 依赖许可证与漏洞审计' cargo deny --manifest-path rust/substrate/Cargo.toml --config rust/substrate/deny.toml --locked check
run_step 'Rust 单元测试' cargo test --locked --manifest-path rust/substrate/Cargo.toml
run_step 'Go 全量测试（race + shuffle）' go test -race -count=1 -shuffle=on -timeout=900s -skip '^(TestFSM_Spec|TestSpec.*)$' ./internal/... ./cmd/... ./pkg/...
run_step '完整源码构建（Go + Rust + Web）' make build
run_step 'Linux/Windows/macOS Go 交叉编译' cross_compile
run_step '内核完整性清单未漂移' check_manifest_clean
run_step '生成本机安装包并校验 SHA-256' package_local

printf '\n检查结果\n'
for i in "${!STEP_NAMES[@]}"; do
  printf '%-38s %-5s %4ss  %s\n' "${STEP_NAMES[$i]}" "${STEP_STATUS[$i]}" \
    "${STEP_SECONDS[$i]}" "${STEP_LOGS[$i]}"
done
if (( FAILED )); then
  printf '\n有检查未通过；修复后重新运行，日志保存在 %s\n' "$LOG_DIR" >&2
  exit 1
fi
printf '\n全部公开发布门控通过。\n'
