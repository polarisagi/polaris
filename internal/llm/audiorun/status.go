package audiorun

import (
	"errors"
	"time"
)

// 状态机取值。与 gateway/server/chat 的 AudioState* 常量字面一致（由 cmd 层适配时直接透传，
// cmd 层有测试守住两边不漂移）；本包不 import chat，保持 llm 层不依赖 gateway 层。
const (
	StateNotInstalled = "not_installed" // 资产未下载；前端首次使用时触发 install
	StateUnsupported  = "unsupported"   // 本机硬件画像低于最低配置 / 基准过慢
	StateDownloading  = "downloading"   // 正在下载并校验资产
	StateLoading      = "loading"       // 正在把引擎加载进内存（含首次基准）
	StateReady        = "ready"         // 资产就绪，可用（引擎可能尚未加载或已空闲卸载）
	StateFailed       = "failed"        // 最近一次下载/加载失败
)

// Status 是一份语音能力状态快照，由服务经 StatusSink 汇报。
type Status struct {
	State  string
	Detail string // 面向用户的当前步骤 / 说明
	Error  string // 失败原因（State=failed）
	Reason string // 机器可读原因码（State=unsupported：见 Reason* 常量）

	InstallSizeBytes int64 // State=not_installed：还需下载的总字节数
	Loaded           bool  // 引擎当前是否驻留内存（State=ready）

	// Origin 标识发起方："auto"|"user"，仅 downloading|loading 时填写。
	Origin string
	// NextRetryAt 处于退避等待时填写。
	NextRetryAt time.Time

	// 下载进度（State=downloading）；BytesTotal 为 0 表示总量未知。
	BytesDone, BytesTotal int64
}

// StatusSink 接收状态快照。接口在本包（调用方）定义，由 cmd 层适配到 chat 的状态机（HE-3）。
type StatusSink interface {
	Publish(Status)
}

// NotReadyError 表示"此刻不能服务该请求"，且原因对用户有意义（HTTP 层映射为 503/422）。
// 它不是内部故障：未安装、硬件不支持、内存不足都属于可向用户解释并可恢复的状态。
type NotReadyError struct {
	Code    string // 见 Code* 常量
	Message string
}

// NotReadyError.Code 取值。
const (
	CodeNotInstalled       = "not_installed"
	CodeInstalling         = "installing"
	CodeUnsupported        = "unsupported"
	CodeInsufficientMemory = "insufficient_memory"
	CodeLoadTimeout        = "loading_timeout"
)

func (e *NotReadyError) Error() string { return e.Message }

// AudioReason 满足 gateway chat 包定义的消费端接口（HTTP 层据此选状态码与 JSON 错误码）。
func (e *NotReadyError) AudioReason() (code, message string) { return e.Code, e.Message }

func notReady(code, msg string) error { return &NotReadyError{Code: code, Message: msg} }

// AsNotReady 提取 err 链中的 *NotReadyError。
func AsNotReady(err error) (*NotReadyError, bool) {
	var nr *NotReadyError
	if errors.As(err, &nr) {
		return nr, true
	}
	return nil, false
}
