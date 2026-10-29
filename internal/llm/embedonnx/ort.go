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
	handle, err := dlopen(dylibPath)
	if err != nil {
		return nil, err
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

func (s *OrtSession) validateInputs(inputIDs []int64, attnMask []int64, tokenTypeIDs []int64) error {
	seqLen := len(inputIDs)
	if seqLen == 0 || len(attnMask) != seqLen {
		return apperr.New(apperr.CodeInvalidInput, "embedonnx: empty or mismatched input tensors")
	}
	if !s.isGemma && len(tokenTypeIDs) != seqLen {
		return apperr.New(apperr.CodeInvalidInput, "embedonnx: empty or mismatched input tensors")
	}
	return nil
}

func (s *OrtSession) createInt64Tensor(data []int64, name string) (uintptr, error) {
	seqLen := int64(len(data))
	shape := []int64{1, seqLen}
	var val uintptr
	// 7 = ONNX_TENSOR_ELEMENT_DATA_TYPE_INT64
	status := s.api.createTensorWithDataAsOrtValue(s.memInfo, unsafe.Pointer(&data[0]), uintptr(seqLen*8), &shape[0], 2, 7, &val)
	if status != 0 {
		msg := goString(s.api.getErrorMessage(status))
		s.api.releaseStatus(status)
		return 0, apperr.New(apperr.CodeInternal, "embedonnx: create "+name+" tensor error: "+msg)
	}
	return val, nil
}

// Run 运行单条推理，返回 512 维 L2 归一化向量。
func (s *OrtSession) Run(inputIDs []int64, attnMask []int64, tokenTypeIDs []int64) ([]float32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.session == 0 {
		return nil, apperr.New(apperr.CodeInternal, "embedonnx: session is closed")
	}

	if err := s.validateInputs(inputIDs, attnMask, tokenTypeIDs); err != nil {
		return nil, err
	}

	idVal, err := s.createInt64Tensor(inputIDs, "input_ids")
	if err != nil {
		return nil, err
	}
	defer s.api.releaseValue(idVal)

	maskVal, err := s.createInt64Tensor(attnMask, "attention_mask")
	if err != nil {
		return nil, err
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
		typeVal, err := s.createInt64Tensor(tokenTypeIDs, "token_type_ids")
		if err != nil {
			return nil, err
		}
		defer s.api.releaseValue(typeVal)

		inNames = []*byte{cString("input_ids"), cString("attention_mask"), cString("token_type_ids")}
		inVals = []uintptr{idVal, maskVal, typeVal}
		outNames = []*byte{cString("last_hidden_state")}
		outVals = make([]uintptr, 1)
	}

	status := s.api.run(s.session, 0, &inNames[0], &inVals[0], uintptr(len(inNames)), &outNames[0], uintptr(len(outNames)), &outVals[0])
	// OrtValue 直接引用 Go 侧 inputIDs/attnMask/tokenTypeIDs 的底层数组（CreateTensorWithDataAsOrtValue
	// 不拷贝），名字数组也只以指针形式交给 C。Go 编译器在最后一次显式使用后即可视其为死对象，
	// 必须保活到 Run 返回之后，否则 GC 可能在推理期间回收输入缓冲区。
	runtime.KeepAlive(inputIDs)
	runtime.KeepAlive(attnMask)
	runtime.KeepAlive(tokenTypeIDs)
	runtime.KeepAlive(inNames)
	runtime.KeepAlive(outNames)
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

	return s.extractEmbedding(outVals)
}

func (s *OrtSession) extractEmbedding(outVals []uintptr) ([]float32, error) {
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

	normalizeL2(emb512)
	return emb512, nil
}

func normalizeL2(vec []float32) {
	var norm float32
	for _, v := range vec {
		norm += v * v
	}
	norm = float32(math.Sqrt(float64(norm)))
	if norm > 0 {
		for i := range vec {
			vec[i] /= norm
		}
	}
}
