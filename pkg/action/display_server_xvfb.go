package action

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"os/exec"

	perrors "github.com/polarisagi/polaris/internal/errors"
)

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
		return perrors.New(perrors.CodeInternal, "xvfb: invalid action format")
	}

	actType, _ := m["type"].(string)
	vec, ok := m["vector"].([]float64)
	if !ok {
		return perrors.New(perrors.CodeInternal, "xvfb: invalid action vector")
	}

	var args []string
	switch actType {
	case "mouse_move":
		if len(vec) < 2 {
			return perrors.New(perrors.CodeInternal, "xvfb: mouse_move requires x, y")
		}
		args = []string{"mousemove", fmt.Sprintf("%d", int(vec[0])), fmt.Sprintf("%d", int(vec[1]))}
	case "mouse_click":
		if len(vec) < 1 {
			return perrors.New(perrors.CodeInternal, "xvfb: mouse_click requires button (1=left, 2=middle, 3=right)")
		}
		args = []string{"click", fmt.Sprintf("%d", int(vec[0]))}
	case "mouse_drag":
		// 按住鼠标移动 (mousedown -> mousemove -> mouseup) 简单示例不支持复杂轨迹
		slog.Warn("xvfb: mouse_drag not fully supported, doing move", "err", perrors.New(perrors.CodeInternal, "log event"))
		if len(vec) < 2 {
			return perrors.New(perrors.CodeInternal, "xvfb: mouse_drag requires x, y")
		}
		args = []string{"mousemove", fmt.Sprintf("%d", int(vec[0])), fmt.Sprintf("%d", int(vec[1]))}
	default:
		return perrors.New(perrors.CodeInternal, fmt.Sprintf("xvfb: unsupported action type %q", actType))
	}

	cmd := exec.Command("xdotool", args...)
	cmd.Env = append(os.Environ(), "DISPLAY="+s.displayID)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return perrors.Wrap(perrors.CodeInternal, fmt.Sprintf("xdotool error: %v, output: %s", err, string(out)), err)
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
		return nil, perrors.Wrap(perrors.CodeInternal, fmt.Sprintf("xwd error: %v", err), err)
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
