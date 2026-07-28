package sandbox

// D4/ADR-0008：长驻会话协议实现。
//
// Python 用一个常驻的小型"harness"脚本自己实现协议（JSON 一行请求 → JSON 一行
// 响应），而不是把用户代码直接喂给 `python3 -i` 交互式 REPL——REPL 的 `>>>`/`...`
// 提示符和"空行才结束多行块"规则难以从管道侧可靠解析（尤其是用户代码本身包含
// 多层缩进/多个函数定义时），JSON 单行响应从根本上避免了这类脆弱的输出解析。
//
// Bash 没有这个问题：bash 以非交互脚本模式（`bash --noprofile --norc -s`）读取
// stdin 时，会把每一批喂进去的命令当作脚本的延续顺序执行、保留变量/函数/cwd，
// 不需要额外的 harness——直接在每次调用的代码末尾追加一行哨兵 echo 即可（见
// sandbox_persistent_session.go execBash）。

// pythonSessionHarness 是长驻 Python 解释器进程运行的驱动脚本，通过
// `python3 -u -c <harness>` 启动。协议：
//
//  1. 从 stdin 读一行 JSON：{"code": "<用户代码文本>"}
//  2. 用显式的 globals 字典 exec 用户代码（会话状态即该字典，天然跨多次调用存活，
//     因为解释器进程本身从未退出——这正是本设计相对 pickle 快照的核心优势：
//     文件句柄/线程/数据库连接等不可序列化对象不会被静默丢弃）
//  3. 通过 contextlib.redirect_stdout/redirect_stderr 捕获输出
//  4. 向 stdout 写回一行 JSON：{"ok":..., "output":..., "stderr":..., "error":...}
//
// 已知限制（与文档中一并说明，非本实现遗漏）：
//   - 用户代码内调用 input() 等阻塞式 stdin 读取会挂起该会话（stdin 被协议
//     独占），需要调用方超时熔断（sandbox_persistent_session.go execPython
//     的 ctx 超时正是为此兜底）。
//   - 变量名以 _polaris_ 前缀命名以降低与用户全局变量意外撞名的可能性，但由于
//     用户代码运行在独立的 _polaris_globals 字典中（exec 第二个参数），与 harness
//     脚本自身的模块级命名空间完全隔离，理论上即使不加前缀也不会冲突；加前缀
//     只是纵深防御。
const pythonSessionHarness = `
import sys, io, json, contextlib, traceback

_polaris_globals = {"__name__": "__polaris_session__"}

while True:
    _polaris_line = sys.stdin.readline()
    if not _polaris_line:
        break
    try:
        _polaris_task = json.loads(_polaris_line)
    except Exception:
        continue
    _polaris_code = _polaris_task.get("code", "")
    _polaris_out = io.StringIO()
    _polaris_err = io.StringIO()
    _polaris_ok = True
    _polaris_error = ""
    try:
        with contextlib.redirect_stdout(_polaris_out), contextlib.redirect_stderr(_polaris_err):
            exec(compile(_polaris_code, "<polaris_session>", "exec"), _polaris_globals)
    except SystemExit:
        pass
    except BaseException:
        _polaris_ok = False
        _polaris_error = traceback.format_exc()
    _polaris_result = {
        "ok": _polaris_ok,
        "output": _polaris_out.getvalue(),
        "stderr": _polaris_err.getvalue(),
        "error": _polaris_error,
    }
    sys.stdout.write(json.dumps(_polaris_result) + "\n")
    sys.stdout.flush()
`
