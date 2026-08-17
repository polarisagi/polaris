# L-10 bounded-cache 棘轮基线

判据见 `tools/bounded_cache_check.go` 头部。格式：每行以 `path:line` 开头，其后为理由。
**只禁增量**——新写的 `bufio.Scanner.Buffer` 上限必须引用 `internal/config` 阀值。

## 背景（2026-08-17）

判据重写前只认 `*ast.BasicLit`，而仓库里的实际写法全是 `1024*1024` 这类 BinaryExpr，
故本规则自诞生起从未报过一次红。重写后一次性暴露 7 处存量，其中 3 处当轮修复
（两条 MCP 传输路径 + LLM 流式解码，均为外部输入路径），余下 4 处按下述理由入基线。

## 存量

- internal/eval/benchmark/benchmark.go:59 评测数据集 JSONL 读取（10 MiB/行）。输入是本地
  数据集文件，不是外部输入面；且离线评测路径与运行时阀值体系无关，接 config 只会给
  M12Eval 增加一个没人会调的旋钮。
- internal/eval/harness/benchmark/swebench.go:92 同上（SWE-bench 数据集，10 MiB/行）。
- internal/eval/harness/benchmark/gaia.go:45 同上（GAIA 数据集，4 MiB/行）。
- cmd/polaris/cli.go:402 本地交互式 stdin 读取（64 KiB/行）。输入来自终端用户自己，
  进程入口层不引 config 阀值与 cmd/ 的既有取向一致（参见 panic_lint 对 cmd/ 的豁免）。

若上述任一条改为读取外部/远端输入，必须同时从本基线移除并接 config 阀值。
