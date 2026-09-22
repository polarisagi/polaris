//go:build wasip1

package probe

// cpuSampleState wasip1 无系统级 CPU 统计接口（沙箱内不可见宿主负载）。
type cpuSampleState struct{}

// sampleCPU 恒定返回 ok=false：宿主 CPU 占用率在 wasm 沙箱内不可观测。
// 调用方拿到 0，等价于"无压力"——沙箱内的插件本就不该按宿主负载做准入决策，
// 那是宿主侧 ResourceGovernor 的职责。
func sampleCPU(prev cpuSampleState) (pct float64, next cpuSampleState, ok bool) {
	return 0, prev, false
}
