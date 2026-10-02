package embedonnx

import (
	"math"
	"runtime"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"

	"github.com/polarisagi/polaris/pkg/apperr"
)

type ortApiBase struct {
	GetApi           uintptr
	GetVersionString uintptr
}

// ORTApi 封装 ORT C API 函数指针。
// 偏移基于 ONNX Runtime 1.23.2 C API 严格对齐（clang offsetof 验证）。
type ORTApi struct {
	createEnv                        func(logLevel int, logID *byte, out *uintptr) uintptr
	createSessionOptions             func(out *uintptr) uintptr
	setIntraOpNumThreads             func(opts uintptr, threads int) uintptr
	setInterOpNumThreads             func(opts uintptr, threads int) uintptr
	setSessionGraphOptimizationLevel func(opts uintptr, level int) uintptr
	createSession                    func(env uintptr, modelPath *byte, opts uintptr, out *uintptr) uintptr
	createCpuMemoryInfo              func(allocType int, memType int, out *uintptr) uintptr
	createTensorWithDataAsOrtValue   func(memInfo uintptr, data unsafe.Pointer, dataLen uintptr, shape *int64, shapeLen uintptr, dataType int, out *uintptr) uintptr
	run                              func(session uintptr, runOpts uintptr, inputNames **byte, inputValues *uintptr, numInputs uintptr, outputNames **byte, numOutputs uintptr, outputValues *uintptr) uintptr
	getTensorMutableData             func(val uintptr, out *unsafe.Pointer) uintptr
	releaseEnv                       func(env uintptr)
	releaseSessionOptions            func(opts uintptr)
	releaseSession                   func(session uintptr)
	releaseMemoryInfo                func(memInfo uintptr)
	releaseValue                     func(val uintptr)
	releaseStatus                    func(status uintptr)
	getErrorMessage                  func(status uintptr) *byte
}

// OpenORT 从 dylib/so/dll 动态库加载 ORT C API。
func OpenORT(dylibPath string) (*ORTApi, error) {
	handle, err := purego.Dlopen(dylibPath, purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "embedonnx: dlopen failed", err)
	}

	var ortGetApiBase func() *ortApiBase
	purego.RegisterLibFunc(&ortGetApiBase, handle, "OrtGetApiBase")

	base := ortGetApiBase()
	if base == nil {
		return nil, apperr.New(apperr.CodeInternal, "embedonnx: OrtGetApiBase returned nil")
	}

	var getApi func(version uint32) uintptr
	purego.RegisterFunc(&getApi, base.GetApi)

	apiPtr := getApi(23)
	if apiPtr == 0 {
		return nil, apperr.New(apperr.CodeInternal, "embedonnx: GetApi(23) returned 0")
	}

	fnPtr := func(offset uintptr) uintptr {
		return *(*uintptr)(unsafe.Pointer(apiPtr + offset))
	}

	api := &ORTApi{}
	purego.RegisterFunc(&api.getErrorMessage, fnPtr(16))
	purego.RegisterFunc(&api.createEnv, fnPtr(24))
	purego.RegisterFunc(&api.createSession, fnPtr(56))
	purego.RegisterFunc(&api.run, fnPtr(72))
	purego.RegisterFunc(&api.createSessionOptions, fnPtr(80))
	purego.RegisterFunc(&api.setSessionGraphOptimizationLevel, fnPtr(184))
	purego.RegisterFunc(&api.setIntraOpNumThreads, fnPtr(192))
	purego.RegisterFunc(&api.setInterOpNumThreads, fnPtr(200))
	purego.RegisterFunc(&api.createTensorWithDataAsOrtValue, fnPtr(392))
	purego.RegisterFunc(&api.getTensorMutableData, fnPtr(408))
	purego.RegisterFunc(&api.createCpuMemoryInfo, fnPtr(552))
	purego.RegisterFunc(&api.releaseEnv, fnPtr(736))
	purego.RegisterFunc(&api.releaseStatus, fnPtr(744))
	purego.RegisterFunc(&api.releaseMemoryInfo, fnPtr(752))
	purego.RegisterFunc(&api.releaseSession, fnPtr(760))
	purego.RegisterFunc(&api.releaseValue, fnPtr(768))
	purego.RegisterFunc(&api.releaseSessionOptions, fnPtr(800))

	return api, nil
}

func cString(s string) *byte {
	b := append([]byte(s), 0)
	return &b[0]
}

func goString(p *byte) string {
	if p == nil {
		return ""
	}
	var b []byte
	for {
		c := *p
		if c == 0 {
			break
		}
		b = append(b, c)
		p = (*byte)(unsafe.Pointer(uintptr(unsafe.Pointer(p)) + 1))
	}
	return string(b)
}

// OrtSession 管理单个模型的推理 Session。
type OrtSession struct {
	api     *ORTApi
	env     uintptr
	opts    uintptr
	session uintptr
	memInfo uintptr
	mu      sync.Mutex
	isGemma bool
}

// NewOrtSession 创建并初始化 ONNX 推理 Session。
func NewOrtSession(api *ORTApi, modelPath string, isGemma bool) (*OrtSession, error) {
	var env uintptr
	status := api.createEnv(3, cString("polaris-embed"), &env)
	if status != 0 {
		msg := goString(api.getErrorMessage(status))
		api.releaseStatus(status)
		return nil, apperr.New(apperr.CodeInternal, "embedonnx: createEnv failed: "+msg)
	}

	var opts uintptr
	status = api.createSessionOptions(&opts)
	if status != 0 {
		api.releaseEnv(env)
		return nil, apperr.New(apperr.CodeInternal, "embedonnx: createSessionOptions failed")
	}

	// 线程策略：前台查询线程 = min(2, max(1, 逻辑核/4))，inter_op = 1，图优化 ORT_ENABLE_ALL
	intraThreads := min(2, max(1, runtime.NumCPU()/4))
	api.setIntraOpNumThreads(opts, intraThreads)
	api.setInterOpNumThreads(opts, 1)
	api.setSessionGraphOptimizationLevel(opts, 99) // ORT_ENABLE_ALL

	var session uintptr
	status = api.createSession(env, cString(modelPath), opts, &session)
	if status != 0 {
		msg := goString(api.getErrorMessage(status))
		api.releaseStatus(status)
		api.releaseSessionOptions(opts)
		api.releaseEnv(env)
		return nil, apperr.New(apperr.CodeInternal, "embedonnx: createSession failed: "+msg)
	}

	var memInfo uintptr
	status = api.createCpuMemoryInfo(0, 0, &memInfo)
	if status != 0 {
		api.releaseSession(session)
		api.releaseSessionOptions(opts)
		api.releaseEnv(env)
		return nil, apperr.New(apperr.CodeInternal, "embedonnx: createCpuMemoryInfo failed")
	}

	return &OrtSession{
		api:     api,
		env:     env,
		opts:    opts,
		session: session,
		memInfo: memInfo,
		isGemma: isGemma,
	}, nil
}

// Close 释放 session 占用的 ORT 原生资源。
func (s *OrtSession) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.session != 0 {
		s.api.releaseSession(s.session)
		s.session = 0
	}
	if s.memInfo != 0 {
		s.api.releaseMemoryInfo(s.memInfo)
		s.memInfo = 0
	}
	if s.opts != 0 {
		s.api.releaseSessionOptions(s.opts)
		s.opts = 0
	}
	if s.env != 0 {
		s.api.releaseEnv(s.env)
		s.env = 0
	}
}

// Run 运行单条推理，返回 512 维 L2 归一化向量。
func (s *OrtSession) Run(inputIDs []int64, attnMask []int64, tokenTypeIDs []int64) ([]float32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.session == 0 {
		return nil, apperr.New(apperr.CodeInternal, "embedonnx: session is closed")
	}

	seqLen := int64(len(inputIDs))
	shape := []int64{1, seqLen}

	var idVal, maskVal, typeVal uintptr
	// 7 = ONNX_TENSOR_ELEMENT_DATA_TYPE_INT64
	status := s.api.createTensorWithDataAsOrtValue(s.memInfo, unsafe.Pointer(&inputIDs[0]), uintptr(seqLen*8), &shape[0], 2, 7, &idVal)
	if status != 0 {
		msg := goString(s.api.getErrorMessage(status))
		s.api.releaseStatus(status)
		return nil, apperr.New(apperr.CodeInternal, "embedonnx: create input_ids tensor error: "+msg)
	}
	defer s.api.releaseValue(idVal)

	status = s.api.createTensorWithDataAsOrtValue(s.memInfo, unsafe.Pointer(&attnMask[0]), uintptr(seqLen*8), &shape[0], 2, 7, &maskVal)
	if status != 0 {
		msg := goString(s.api.getErrorMessage(status))
		s.api.releaseStatus(status)
		return nil, apperr.New(apperr.CodeInternal, "embedonnx: create attention_mask tensor error: "+msg)
	}
	defer s.api.releaseValue(maskVal)

	var inNames []*byte
	var inVals []uintptr
	var outNames []*byte
	var outVals []uintptr

	if s.isGemma {
		inNames = []*byte{cString("input_ids"), cString("attention_mask")}
		inVals = []uintptr{idVal, maskVal}
		outNames = []*byte{cString("last_hidden_state"), cString("sentence_embedding")}
		outVals = make([]uintptr, 2)
	} else {
		status = s.api.createTensorWithDataAsOrtValue(s.memInfo, unsafe.Pointer(&tokenTypeIDs[0]), uintptr(seqLen*8), &shape[0], 2, 7, &typeVal)
		if status != 0 {
			msg := goString(s.api.getErrorMessage(status))
			s.api.releaseStatus(status)
			return nil, apperr.New(apperr.CodeInternal, "embedonnx: create token_type_ids tensor error: "+msg)
		}
		defer s.api.releaseValue(typeVal)

		inNames = []*byte{cString("input_ids"), cString("attention_mask"), cString("token_type_ids")}
		inVals = []uintptr{idVal, maskVal, typeVal}
		outNames = []*byte{cString("last_hidden_state")}
		outVals = make([]uintptr, 1)
	}

	status = s.api.run(s.session, 0, &inNames[0], &inVals[0], uintptr(len(inNames)), &outNames[0], uintptr(len(outNames)), &outVals[0])
	if status != 0 {
		msg := goString(s.api.getErrorMessage(status))
		s.api.releaseStatus(status)
		return nil, apperr.New(apperr.CodeInternal, "embedonnx: run session error: "+msg)
	}

	defer func() {
		for _, val := range outVals {
			if val != 0 {
				s.api.releaseValue(val)
			}
		}
	}()

	emb512 := make([]float32, 512)
	if s.isGemma {
		// outVals[1] 是 sentence_embedding [1, 768]
		var dataPtr unsafe.Pointer
		s.api.getTensorMutableData(outVals[1], &dataPtr)
		if dataPtr == nil {
			return nil, apperr.New(apperr.CodeInternal, "embedonnx: nil tensor data for gemma")
		}
		rawEmb := (*[768]float32)(dataPtr)
		// Matryoshka 前 512 维截断
		copy(emb512, rawEmb[:512])
	} else {
		// outVals[0] 是 last_hidden_state [1, seqLen, 512]
		var dataPtr unsafe.Pointer
		s.api.getTensorMutableData(outVals[0], &dataPtr)
		if dataPtr == nil {
			return nil, apperr.New(apperr.CodeInternal, "embedonnx: nil tensor data for bge")
		}
		rawCLS := (*[512]float32)(dataPtr)
		copy(emb512, rawCLS[:512])
	}

	// L2 归一化
	var norm float32
	for j := 0; j < 512; j++ {
		norm += emb512[j] * emb512[j]
	}
	norm = float32(math.Sqrt(float64(norm)))
	if norm > 0 {
		for j := 0; j < 512; j++ {
			emb512[j] /= norm
		}
	}

	return emb512, nil
}
