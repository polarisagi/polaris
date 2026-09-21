// Package runtimeinfo 读写守护进程的运行时状态文件（端口 / 令牌 / PID）。
//
// 它是客户端（CLI、桌面外壳）发现本机守护进程的唯一途径：守护进程监听成功后写入
// 实际端口与本次启动生成的本地令牌，客户端据此连接（ADR-0096 决策五/决策六）。
//
// # 为什么只管读写不管路径
//
// 路径 SSoT 是 internal/config.DataLayout（见该文件头「所有子系统必须从此结构取路径」）。
// 本包接收绝对路径、不推导路径——否则同一组路径会存在两份拼接实现，改布局时必然漂移
// 其一，而漂移的那一份表现为"客户端连不上一个正在运行的服务"，无任何报错线索。
//
// # 为什么不在这里探活
//
// 判定"进程是否还活着"由 cmd/polaris 的单实例文件锁承担，不用 PID 探活：PID 会被
// 操作系统复用，陈旧 pid 文件恰好撞上一个无关进程时，探活会给出"在运行"的错误结论。
// 锁由内核在进程退出时自动释放，不存在这个失真面。本包写 PID 仅供人工排查与
// `polaris service status` 展示，不作为任何控制流判据。
package runtimeinfo

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// filePerm 是三个运行时文件的权限。令牌等价于完整 API 权限，故同组/其他用户可读即
// 等于本机任意账号可接管守护进程——读取方发现权限放宽时按失败处理，不做"宽容读取"。
const filePerm os.FileMode = 0o600

// tokenBytes 是本地令牌的随机字节数（hex 编码后 64 字符）。
const tokenBytes = 32

// Paths 是 run/ 下三个运行时文件的绝对路径，由 config.DataLayout 提供。
type Paths struct {
	PID   string
	Port  string
	Token string
}

// State 是一次守护进程运行的可发现状态。
type State struct {
	PID   int
	Port  int
	Token string
}

// NewToken 生成一枚本地令牌。每次启动轮换，不落盘复用——令牌的生命周期与进程一致，
// 持久化它只会让一次泄露长期有效。
func NewToken() (string, error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "runtimeinfo: 生成本地令牌失败", err)
	}
	return hex.EncodeToString(b), nil
}

// Write 原子写入三个运行时文件。调用方须先建好父目录（config.DataLayout.MkdirAll）。
func Write(p Paths, s State) error {
	if s.Port <= 0 || s.Port > 65535 {
		return apperr.New(apperr.CodeInvalidInput, "runtimeinfo: 端口越界 "+strconv.Itoa(s.Port))
	}
	if s.Token == "" {
		return apperr.New(apperr.CodeInvalidInput, "runtimeinfo: 令牌为空")
	}
	items := []struct {
		path string
		data string
	}{
		{p.PID, strconv.Itoa(s.PID)},
		{p.Port, strconv.Itoa(s.Port)},
		{p.Token, s.Token},
	}
	for _, it := range items {
		if err := writeAtomic(it.path, it.data); err != nil {
			return err
		}
	}
	return nil
}

// writeAtomic 同目录临时文件 + rename 落盘。
// 不用「截断后重写」：客户端可能恰好在两次写之间读到半截端口号，
// 而半截端口号是一个合法整数——错误会表现为连接被拒，而非解析失败。
func writeAtomic(path, data string) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "runtimeinfo: 创建临时文件失败 "+path, err)
	}
	tmp := f.Name()
	defer func() {
		// rename 成功后临时文件已不存在，这里是空操作；真正删不掉（如目录权限
		// 被改）必须留痕，否则 run/ 下会悄悄堆积残渣而无人知情（HE-1）。
		if rmErr := os.Remove(tmp); rmErr != nil && !os.IsNotExist(rmErr) {
			slog.Warn("runtimeinfo: 临时文件清理失败", "path", tmp, "err", rmErr)
		}
	}()

	if _, err = f.WriteString(data); err != nil {
		_ = f.Close()
		return apperr.Wrap(apperr.CodeInternal, "runtimeinfo: 写入失败 "+path, err)
	}
	if err = f.Close(); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "runtimeinfo: 关闭临时文件失败 "+path, err)
	}
	// 先改权限再 rename：反过来的话，目标路径会有一个短暂的 0600 之前的窗口，
	// 而这个窗口里落地的恰好是令牌。
	if err = os.Chmod(tmp, filePerm); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "runtimeinfo: 设置权限失败 "+path, err)
	}
	if err = os.Rename(tmp, path); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "runtimeinfo: 落盘失败 "+path, err)
	}
	return nil
}

// Read 读取端口与令牌。PID 缺失或损坏不视为失败（它不参与控制流，见包注释）。
//
// 错误码对应 ADR-0096 决策七的启动判定分支，调用方按码分流，不要按字符串匹配：
//   - CodeNotFound     文件不存在 → 守护进程未运行（或未曾运行）
//   - CodeForbidden    权限宽于 0600 → 令牌可能已被他人读取，拒绝使用
//   - CodeInvalidInput 内容损坏 → 陈旧或被截断的文件
func Read(p Paths) (State, error) {
	port, err := readInt(p.Port)
	if err != nil {
		return State{}, err
	}
	if port <= 0 || port > 65535 {
		return State{}, apperr.New(apperr.CodeInvalidInput, "runtimeinfo: 端口文件内容越界 "+p.Port)
	}
	token, err := readSecret(p.Token)
	if err != nil {
		return State{}, err
	}
	pid, err := readInt(p.PID)
	if err != nil {
		pid = 0 // 仅用于展示，读不到不影响连接
	}
	return State{PID: pid, Port: port, Token: token}, nil
}

// Remove 清理三个文件，用于优雅关停。逐个尽力删除，不因其一失败而跳过其余——
// 残留的端口文件会让下一个客户端连向一个已经不存在的端口。
func Remove(p Paths) {
	for _, path := range []string{p.PID, p.Port, p.Token} {
		if path == "" {
			continue
		}
		// 文件本就不存在不算失败（重复关停、启动早期失败）；其余失败必须留痕：
		// 残留的端口文件会让下一个客户端连向一个已经不存在的端口。
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			slog.Warn("runtimeinfo: 清理运行时文件失败", "path", path, "err", err)
		}
	}
}

// readInt 读取一个仅含十进制整数的文件。
func readInt(path string) (int, error) {
	b, err := os.ReadFile(path) //nolint:gosec // 路径来自 config.DataLayout，非外部输入
	if err != nil {
		if os.IsNotExist(err) {
			return 0, apperr.Wrap(apperr.CodeNotFound, "runtimeinfo: 文件不存在 "+path, err)
		}
		return 0, apperr.Wrap(apperr.CodeInternal, "runtimeinfo: 读取失败 "+path, err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0, apperr.Wrap(apperr.CodeInvalidInput, "runtimeinfo: 内容非整数 "+path, err)
	}
	return n, nil
}

// readSecret 读取令牌文件，并校验其权限未被放宽。
func readSecret(path string) (string, error) {
	fi, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", apperr.Wrap(apperr.CodeNotFound, "runtimeinfo: 文件不存在 "+path, err)
		}
		return "", apperr.Wrap(apperr.CodeInternal, "runtimeinfo: 读取状态失败 "+path, err)
	}
	if err = checkPerm(fi.Mode().Perm(), path); err != nil {
		return "", err
	}
	b, err := os.ReadFile(path) //nolint:gosec // 同 readInt
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "runtimeinfo: 读取失败 "+path, err)
	}
	token := strings.TrimSpace(string(b))
	if token == "" {
		return "", apperr.New(apperr.CodeInvalidInput, "runtimeinfo: 令牌文件为空 "+path)
	}
	return token, nil
}

// checkPerm 拒绝宽于 0600 的令牌文件。
//
// Windows 上 Go 只把文件模拟成 0666/0444 两种权限位，与 ACL 无关，按位判定必然
// 恒假——那样这条校验在 Windows 上就成了一个永远绿灯的门控，比没有更糟（它会让
// 人以为已经校验过）。故在 Windows 上显式跳过，权限由 NTFS ACL 承担。
func checkPerm(perm os.FileMode, path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	if perm&0o077 != 0 {
		return apperr.New(apperr.CodeForbidden,
			"runtimeinfo: 令牌文件权限过宽（"+perm.String()+"，要求 -rw-------）"+path)
	}
	return nil
}
