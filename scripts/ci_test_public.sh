#!/usr/bin/env bash
# 公开项目的本地发布门控；与 .github/workflows/ci.yml 使用相同检查入口。
set -u
set -o pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$ROOT" || exit 2

STAMP=$(date +%Y%m%d-%H%M%S)
LOG_DIR=${CI_LOG_DIR:-"$ROOT/.ci-logs/public-$STAMP"}
mkdir -p "$LOG_DIR" || exit 2
if [[ "$LOG_DIR" == "$ROOT/.ci-logs/"* ]]; then
  ln -sfn "$(basename "$LOG_DIR")" "$ROOT/.ci-logs/latest"
fi
TMP_DIR=$(mktemp -d "${TMPDIR:-/tmp}/polaris-public-ci.XXXXXX") || exit 2
INTERRUPTED=0
trap 'rm -rf "$TMP_DIR"' EXIT
trap 'INTERRUPTED=1' INT

STEP_NAMES=()
STEP_STATUS=()
STEP_CODES=()
STEP_SECONDS=()
STEP_LOGS=()
STEP_CMDS=()
FAILED_IDX=()

# 从步骤日志中按输出格式提取错误。只用 POSIX awk 特性（macOS 为 BWK awk）。
# 分节输出；任何专用节都没命中时才回落到关键词匹配 + 日志末尾。
EXTRACT_AWK='
function add(n, s) { c[n]++; L[n, c[n] % MAXL] = s }
function dump(n, ind,   i, s, r) {
    r = ""
    if (!(n in c)) return r
    s = 1
    if (c[n] > MAXL) { r = ind "…（省略前 " (c[n] - MAXL) " 行，见完整日志）\n"; s = c[n] - MAXL + 1 }
    for (i = s; i <= c[n]; i++) r = r ind L[n, i % MAXL] "\n"
    return r
}
function drop(n,   i) {
    if (!(n in c)) return
    for (i = 0; i < MAXL; i++) delete L[n, i]
    delete c[n]
}
function addh(n, s) { hc[n]++; if (hc[n] <= MAXH) H[n, hc[n]] = s }
function dumph(n, ind,   i, m, r) {
    r = ""
    if (!(n in hc)) return r
    m = hc[n] < MAXH ? hc[n] : MAXH
    for (i = 1; i <= m; i++) r = r ind H[n, i] "\n"
    if (hc[n] > MAXH) r = r ind "…（省略后 " (hc[n] - MAXH) " 行，见完整日志）\n"
    return r
}
function reset_pkg(   k, n, i, keys) {
    n = 0
    for (k in c) keys[++n] = k
    for (i = 1; i <= n; i++) drop(keys[i])
    delete H; delete hc; delete isfail; delete fl
    nfail = 0; nrace = 0; cur = ""; inpanic = 0; inrace = 0; sep = 0
}
function glob(sec, s) { G[sec] = G[sec] s "\n"; GN[sec]++ }
function on_pkg_fail(line,   pkg, i, n, r, a) {
    split(line, a, "\t"); pkg = a[2]
    npkg++
    r = "▸ 包 " pkg "\n"
    if (line ~ /\[(build|setup) failed\]/) {
        r = r "    编译/装配失败，见「编译 / 静态检查问题」节\n"
    }
    for (i = 1; i <= nfail; i++) {
        n = fl[i]; ntest++
        r = r "  ✗ " n "\n" dump(n, "      ")
    }
    if ("__panic" in hc) r = r "  ⚡ panic / fatal error:\n" dumph("__panic", "      ")
    for (i = 1; i <= nrace && i <= 3; i++) r = r "  ⚠ DATA RACE #" i ":\n" dumph("__race" i, "      ")
    if (nrace > 3) r = r "  ⚠ 另有 " (nrace - 3) " 处 DATA RACE，见完整日志\n"
    if (nfail == 0 && !("__panic" in hc) && nrace == 0 && line !~ /\[(build|setup) failed\]/) {
        if (cur != "" && (cur in c)) r = r "  进程在用例 " cur " 执行期间退出，其输出：\n" dump(cur, "      ")
        if ("" in c) r = r "  包级输出：\n" dump("", "      ")
    }
    T = T r
    reset_pkg()
}
BEGIN { MAXL = 80; MAXH = 60; MAXTAIL = 25; MAXKW = 30; nfail = 0; cur = "" }

{
    ntail++; TL[ntail % MAXTAIL] = $0
    if ($0 ~ /FAIL|Error|ERROR|error:|fatal|panic:|❌|✗/) { nkw++; KW[nkw % MAXKW] = $0 }
}

/^=== (RUN|CONT|NAME|PAUSE) / { cur = $3; gotest = 1; next }
/^[ \t]*--- (PASS|SKIP|FAIL): / {
    t = $0; sub(/^[ \t]+/, "", t); split(t, a, " ")
    if (a[2] == "FAIL:") { if (!(a[3] in isfail)) { isfail[a[3]] = 1; fl[++nfail] = a[3] } }
    else drop(a[3])
    next
}
/^FAIL\t/ { on_pkg_fail($0); next }
/^(ok  |\?   )\t/ { reset_pkg(); next }
/^(PASS|FAIL)$/ || /^coverage: / { next }

/^==================$/ { if (inrace) { inrace = 0 } else { sep = 1 }; next }
sep && /^WARNING: DATA RACE/ { sep = 0; inrace = 1; nrace++; nrace_total++; addh("__race" nrace, $0); next }
{ sep = 0 }
inrace { addh("__race" nrace, $0); next }

/^(panic: |fatal error: )/ { inpanic = 1 }
inpanic { addh("__panic", $0); next }

/^# [^ ]/ { lasthash = $0; next }
/^[^ \t:]+\.go:[0-9]+(:[0-9]+)?: / {
    if (lasthash != "") { glob("go", lasthash); lasthash = "" }
    glob("go", $0); ctx = 3; next
}
ctx > 0 && /^([ \t]|\^)/ { ctx--; glob("go", $0); next }
{ ctx = 0 }
/^[0-9]+ issues?:/ || /^\* [a-z0-9]+: [0-9]+/ { glob("go", $0); next }
/level=error/ { glob("go", $0); next }

!gotest && /^error(\[[A-Za-z0-9_-]*\])?: / {
    inerr = 1; nerr++; bl = 0
    if (nerr <= 20) glob("rust", "")
}
inerr && /^[ \t]*$/ { inerr = 0; next }
inerr {
    if (nerr <= 20) { if (bl < 40) glob("rust", $0); else if (bl == 40) glob("rust", "…（块内省略，见完整日志）") }
    bl++; next
}

/^Diff in / { infmt = 1; fl2 = 0 }
infmt && /^[ \t]*$/ { infmt = 0; next }
infmt { if (fl2 < 30) glob("fmt", $0); fl2++; next }

/^diff --git / { indiff = 1 }
indiff && /^(diff --git |index |--- |\+\+\+ |@@|[-+ ])/ {
    if (GN["diff"] < 120) glob("diff", $0); else if (GN["diff"] == 120) glob("diff", "…（diff 过长，见完整日志）")
    next
}
{ indiff = 0 }

/^make(\[[0-9]+\])?: \*\*\* / { glob("make", $0); next }

{ add(cur, $0) }

END {
    any = 0
    if (T != "") { printf "── 失败的 Go 测试（%d 个包 / %d 个用例）──\n%s\n", npkg, ntest, T; any = 1 }
    if (G["go"] != "") { printf "── 编译 / 静态检查问题 ──\n%s\n", G["go"]; any = 1 }
    if (G["rust"] != "") {
        printf "── Rust / cargo 错误（%d 块%s）──%s\n", nerr, (nerr > 20 ? "，仅列前 20" : ""), G["rust"]; any = 1
    }
    if (G["fmt"] != "") { printf "── 格式化差异 ──\n%s\n", G["fmt"]; any = 1 }
    if (G["diff"] != "") { printf "── 未提交的生成物差异 ──\n%s\n", G["diff"]; any = 1 }
    if (G["make"] != "") { printf "── make 失败目标 ──\n%s\n", G["make"]; any = 1 }
    if (!any) {
        if (nkw > 0) {
            print "── 含错误关键词的行（末 " (nkw < MAXKW ? nkw : MAXKW) " 条）──"
            for (i = (nkw > MAXKW ? nkw - MAXKW + 1 : 1); i <= nkw; i++) print KW[i % MAXKW]
            print ""
        }
        print "── 日志末尾 ──"
        for (i = (ntail > MAXTAIL ? ntail - MAXTAIL + 1 : 1); i <= ntail; i++) print TL[i % MAXTAIL]
    }
}
'

extract_errors() {
  tail -n +2 "$1" | LC_ALL=C sed $'s/\033\\[[0-9;]*[A-Za-z]//g' | awk "$EXTRACT_AWK"
}

fmt_secs() {
  local s=$1
  if (( s >= 60 )); then printf '%dm%02ds' $((s / 60)) $((s % 60)); else printf '%ds' "$s"; fi
}

run_step() {
  local name=$1
  shift
  local index=${#STEP_NAMES[@]}
  local slug log_file cmd_str=""
  local started=$SECONDS
  local result=0
  printf -v slug '%02d' "$((index + 1))"
  log_file="$LOG_DIR/$slug.log"
  printf -v cmd_str ' %q' "$@"
  cmd_str=${cmd_str# }

  STEP_NAMES+=("$name")
  STEP_CMDS+=("$cmd_str")
  STEP_LOGS+=("$log_file")

  if (( INTERRUPTED )); then
    STEP_STATUS+=(SKIP)
    STEP_CODES+=("-")
    STEP_SECONDS+=(0)
    return
  fi

  printf '\n\033[1;34m▶ %s...\033[0m\n$ %s\n' "$name" "$cmd_str"
  printf '$ %s\n' "$cmd_str" >"$log_file"

  "$@" >>"$log_file" 2>&1 || result=$?
  local dur=$((SECONDS - started))
  STEP_CODES+=("$result")
  STEP_SECONDS+=("$dur")

  if (( INTERRUPTED )); then
    STEP_STATUS+=(INT)
    FAILED_IDX+=("$index")
    printf '\033[1;33m⏹ 中断: %s (%s)，后续步骤将跳过\033[0m\n' "$name" "$(fmt_secs "$dur")"
  elif (( result == 0 )); then
    STEP_STATUS+=(PASS)
    tail -n +2 "$log_file" | tail -n 12
    printf '\033[1;32m✅ 通过: %s (%s)\033[0m\n' "$name" "$(fmt_secs "$dur")"
  else
    STEP_STATUS+=(FAIL)
    FAILED_IDX+=("$index")
    extract_errors "$log_file" > "$LOG_DIR/$slug.errors.txt"
    tail -n +2 "$log_file" | tail -n 80
    printf '\033[1;31m❌ 失败: %s (退出码 %s, %s)\033[0m\n' "$name" "$result" "$(fmt_secs "$dur")"
    printf '\033[1;31m   完整日志: %s\033[0m\n' "$log_file"
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
    case "${STEP_NAMES[$i]}" in
      *'完整源码构建（Go + Rust + Web）'*)
        [ "${STEP_STATUS[$i]}" = PASS ] && return 0
        printf '完整源码构建未通过，跳过依赖其产物的安装包验证。\n' >&2
        return 1
        ;;
    esac
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

echo "======================================"
echo "    Polaris 本地发布门控完整校验      "
echo "======================================"
echo "日志目录: $LOG_DIR"

run_step '[1/14] 检查构建工具链' check_tools
run_step '[2/14] 检查 Git 差异空白错误' check_diff_clean
# npm audit 只读 package-lock.json，无需先装依赖；依赖安装由 make build → build-ui 的 npm ci 完成，避免装两遍。
run_step '[3/14] 前端依赖漏洞审计' npm audit --prefix web --audit-level=moderate
run_step '[4/14] Go 源码格式检查' check_go_format
# unsafe.Pointer 是 Rust/C FFI 的预期边界；保留其他 vet 检查。
run_step '[5/14] Go 静态分析（排除 FFI unsafe.Pointer 误报）' go vet -unsafeptr=false ./...
run_step '[6/14] Rust 格式检查' cargo fmt --manifest-path rust/substrate/Cargo.toml --check
run_step '[7/14] Rust Clippy 静态检查' cargo clippy --locked --manifest-path rust/substrate/Cargo.toml --all-targets -- -D warnings
run_step '[8/14] Rust 依赖许可证与漏洞审计' cargo deny --manifest-path rust/substrate/Cargo.toml --config rust/substrate/deny.toml --locked check
run_step '[9/14] Rust 单元测试' cargo test --locked --manifest-path rust/substrate/Cargo.toml
run_step '[10/14] Go 全量测试（race + shuffle）' go test -race -count=1 -shuffle=on -timeout=900s -skip '^(TestFSM_Spec|TestSpec.*)$' ./internal/... ./cmd/... ./pkg/...
run_step '[11/14] 完整源码构建（Go + Rust + Web）' make build
run_step '[12/14] Linux/Windows/macOS Go 交叉编译' cross_compile
run_step '[13/14] 内核完整性清单未漂移' check_manifest_clean
run_step '[14/14] 生成本机安装包并校验 SHA-256' package_local

echo ""
echo "======================================"
echo " 步骤汇总"
echo "======================================"
for i in "${!STEP_NAMES[@]}"; do
  case "${STEP_STATUS[$i]}" in
    PASS) color="1;32"; mark="✅" ;;
    FAIL) color="1;31"; mark="❌" ;;
    INT)  color="1;33"; mark="⏹" ;;
    *)    color="0;37"; mark="⏭" ;;
  esac
  printf "\033[%sm %s %-58s 退出码 %-4s %s\033[0m\n" "$color" "$mark" "${STEP_NAMES[$i]}" \
    "${STEP_CODES[$i]}" "$(fmt_secs "${STEP_SECONDS[$i]}")"
done

if [ ${#FAILED_IDX[@]} -eq 0 ]; then
  echo "======================================"
  echo -e "\033[1;32m 🎉 所有公开发布门控已顺利通过！\033[0m"
  echo -e "\033[1;32m 您现在可以放心地推送到 GitHub。\033[0m"
  echo "======================================"
  exit 0
fi

echo ""
echo -e "\033[1;31m ❌ 本次发布门控校验未通过，失败步骤详情：\033[0m"
for i in "${FAILED_IDX[@]}"; do
  slug=$(printf '%02d' $((i + 1)))
  echo ""
  echo -e "\033[1;31m━━━━ ${STEP_NAMES[$i]}（退出码 ${STEP_CODES[$i]}）━━━━\033[0m"
  echo "  命令:     ${STEP_CMDS[$i]}"
  echo "  完整日志: ${STEP_LOGS[$i]}"
  if [ "${STEP_STATUS[$i]}" = "INT" ]; then
    echo "  （被 Ctrl-C 中断，未提取错误）"
    continue
  fi
  echo "  错误摘要: $LOG_DIR/$slug.errors.txt"
  echo -e "\033[1;33m  ────────────── 错误摘要 ──────────────\033[0m"
  sed 's/^/  /' "$LOG_DIR/$slug.errors.txt"
done
echo ""
echo -e "\033[1;33m 请根据上方日志修复报错信息后再尝试推送。\033[0m"
echo "======================================"
if (( INTERRUPTED )); then exit 130; fi
exit 1
