package lam

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// sanitizeX11Env 构造 X11 子进程所需的最小环境变量集合，
// 并将 DISPLAY 覆盖为指定的虚拟显示器（防止泄漏父进程实际桌面 DISPLAY）。
func sanitizeX11Env(overrideDisplay string) []string {
	// x11AllowedEnvKeys 是 xdotool / xwd 等 X11 工具子进程可继承的变量白名单。
	// X11 工具依赖 DISPLAY、XAUTHORITY 及基础运行时变量；其余变量一律拦截（R1.15）。
	allowedKeys := map[string]struct{}{
		"PATH":                     {},
		"HOME":                     {},
		"TMPDIR":                   {},
		"LANG":                     {},
		"LC_ALL":                   {},
		"XAUTHORITY":               {},
		"DBUS_SESSION_BUS_ADDRESS": {},
		"XDG_RUNTIME_DIR":          {},
	}
	raw := os.Environ()
	out := make([]string, 0, len(allowedKeys)+1)
	for _, kv := range raw {
		idx := strings.IndexByte(kv, '=')
		if idx <= 0 {
			continue
		}
		key := strings.ToUpper(kv[:idx])
		if key == "DISPLAY" {
			continue // 下方统一追加 overrideDisplay，避免父进程 DISPLAY 混入
		}
		if _, ok := allowedKeys[key]; ok {
			out = append(out, kv)
		}
	}
	// 强制注入目标虚拟显示器，确保 xdotool 操作正确的 Xvfb 实例
	out = append(out, "DISPLAY="+overrideDisplay)
	return out
}

// XvfbDisplayServer 实现了 DisplayServer 接口，用于 Linux 环境下的无头 GUI 交互（LAM）。
// 依赖系统命令：Xvfb, xdotool, xwd, convert (ImageMagick)。
type XvfbDisplayServer struct {
	displayID string // 例如 ":99"
}

// NewXvfbDisplayServer 创建一个基于 Xvfb 的显示服务端实现。
func NewXvfbDisplayServer(displayID string) *XvfbDisplayServer {
	if displayID == "" {
		displayID = ":99"
	}
	return &XvfbDisplayServer{displayID: displayID}
}

// SendAction 执行 xdotool 命令以发送动作。
// 动作映射：
//   - ActionType = "mouse_move" : vector [x, y]
//   - ActionType = "mouse_click": vector [button]
//   - ActionType = "key_press"  : vector 暂不处理，依赖具体字符串（此处简单映射或略过，按需求定制）
func (s *XvfbDisplayServer) SendAction(action any) error {
	m, ok := action.(map[string]any)
	if !ok {
		return apperr.New(apperr.CodeInternal, "xvfb: invalid action format")
	}

	actType, _ := m["type"].(string)
	vec, ok := m["vector"].([]float64)
	if !ok {
		return apperr.New(apperr.CodeInternal, "xvfb: invalid action vector")
	}

	var args []string
	switch actType {
	case "mouse_move":
		if len(vec) < 2 {
			return apperr.New(apperr.CodeInternal, "xvfb: mouse_move requires x, y")
		}
		args = []string{"mousemove", fmt.Sprintf("%d", int(vec[0])), fmt.Sprintf("%d", int(vec[1]))}
	case "mouse_click":
		if len(vec) < 1 {
			return apperr.New(apperr.CodeInternal, "xvfb: mouse_click requires button (1=left, 2=middle, 3=right)")
		}
		args = []string{"click", fmt.Sprintf("%d", int(vec[0]))}
	case "mouse_drag":
		// 按住鼠标移动 (mousedown -> mousemove -> mouseup) 简单示例不支持复杂轨迹
		slog.Warn("xvfb: mouse_drag not fully supported, doing move", "err", apperr.New(apperr.CodeInternal, "log event"))
		if len(vec) < 2 {
			return apperr.New(apperr.CodeInternal, "xvfb: mouse_drag requires x, y")
		}
		args = []string{"mousemove", fmt.Sprintf("%d", int(vec[0])), fmt.Sprintf("%d", int(vec[1]))}
	default:
		return apperr.New(apperr.CodeInternal, fmt.Sprintf("xvfb: unsupported action type %q", actType))
	}

	// 豁免说明：
	// 此处的 X11 交互工具 (xdotool, xwd, convert) 以及长驻后台进程 (Xvfb) 保留原生 exec.Command 调用，不接入 Rust V2 沙箱。
	// 原因：Rust V2 沙箱主要用于生命周期明确的单次任务执行。若将此类与长驻后台服务紧密交互、或本身就是长驻进程
	// 的组件放入隔离沙箱，可能导致资源泄露、僵尸进程（PID namespace 孤儿）或 X11 状态无法清理（socket 挂载问题）。
	// 且参数均为内部构造的简单坐标指令，无外部 shell 注入风险。
	cmd := exec.Command("xdotool", args...)
	// 使用 X11 白名单环境，并将 DISPLAY 覆盖为目标虚拟显示器（R1.15）
	cmd.Env = sanitizeX11Env(s.displayID)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("xdotool error: %v, output: %s", err, string(out)), err)
	}
	return nil
}

// GetFrame 截取当前 Xvfb 屏幕，使用 xwd 和 convert 输出 PNG。
func (s *XvfbDisplayServer) GetFrame() ([]byte, error) {
	// 使用 xwd 截屏
	xwdCmd := exec.Command("xwd", "-root", "-display", s.displayID)
	var xwdOut bytes.Buffer
	xwdCmd.Stdout = &xwdOut
	if err := xwdCmd.Run(); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("xwd error: %v", err), err)
	}

	// 转换为 PNG (如果安装了 ImageMagick)
	// 由于这只是接口预留实现，这里简化为返回 xwd 数据或转换它。
	// 这里使用 convert - xwd: png:-
	convertCmd := exec.Command("convert", "xwd:-", "png:-")
	convertCmd.Stdin = &xwdOut
	var pngOut bytes.Buffer
	convertCmd.Stdout = &pngOut
	if err := convertCmd.Run(); err != nil {
		// 如果没有 ImageMagick，退化为返回 raw xwd（虽然 M2 识别需要 image/png）
		slog.Warn("xvfb: convert to png failed, returning raw xwd", "err", err)
		return xwdOut.Bytes(), nil
	}

	return pngOut.Bytes(), nil
}
