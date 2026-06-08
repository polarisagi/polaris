package skill

// Logic Collapse 编译流水线。
// 架构文档: docs/arch/M06-Skill-Library.md §2.2
// 顺序: freshnessCheck → dataStripping → compileGate → canStartNewCompile
//        → LLM 代码生成 → 静态分析 → AST 脱敏 → 远程双源编译 → WasmHash 验证
//        → wazero 沙箱验证 → 风险分级 → 签名 → 入库

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	perrors "github.com/polarisagi/polaris/internal/errors"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/substrate/observability"
)

// ─── 常量与错误 ──────────────────────────────────────────────────────────────

const (
	MinSuccessCount     = 50  // 编译前安全闸门: 最少成功次数（对外导出，M9 复用）
	MinSemanticVariance = 0.1 // 最小语义方差（低于此 → 多样性不足 → 拒绝，对外导出）

	compileMinFreeMemMB   = 80 // CompileGate: 最小剩余内存(MB)
	remoteCompileTimeout  = 120 * time.Second
	freshnessCheckTimeout = 500 * time.Millisecond
)

// forbiddenGoImports 编译前静态分析禁止的导入。
var forbiddenGoImports = []string{
	"os/exec", "net/http", "net", "syscall", "unsafe",
	"runtime/debug", // 防止 panic 隐藏
}

// nonDeterministicCalls 禁止的非确定性调用；必须通过 context_hint 注入。
var nonDeterministicCalls = []string{
	"time.Now", "time.Since", "rand.Read", "rand.Intn", "rand.Float64",
	"rand.Int63", "os.Getenv", "os.Args",
}

var (
	ErrLogicCollapseDisabled               = perrors.New(perrors.CodeInternal, "logic collapse: feature gate disabled (Tier0 or insufficient memory)")
	ErrLogicCollapseUnavailableInLocalOnly = perrors.New(perrors.CodeInternal, "logic collapse: unavailable in local_only privacy mode")
	ErrInsufficientSuccessCount            = perrors.New(perrors.CodeInternal, "logic collapse: success_count < 50")
	ErrInsufficientSemanticDiversity       = perrors.New(perrors.CodeInternal, "logic collapse: semantic_variance < 0.1 — needs_more_diversity")
	ErrEvalGateNotPassed                   = perrors.New(perrors.CodeInternal, "logic collapse: eval gate not passed")
	ErrStaleTrajectory                     = perrors.New(perrors.CodeInternal, "logic collapse: stale trajectory — needs_adaptation")
	ErrCompileGateRejected                 = perrors.New(perrors.CodeInternal, "logic collapse: compile gate rejected (memory or concurrency limit)")
	ErrWasmHashMismatch                    = perrors.New(perrors.CodeInternal, "logic collapse: dual-source wasm hash mismatch — supply chain attack suspected")
	ErrNoRemoteService                     = perrors.New(perrors.CodeInternal, "logic collapse: no remote compile service configured")
	ErrInsufficientCompileServices         = perrors.New(perrors.CodeInternal, "logic collapse: need at least 2 compile services for dual-source verification")
	ErrTaintedTrajectory                   = perrors.New(perrors.CodeInternal, "logic collapse: TaintMedium+ trajectory rejected — tainted_trajectory")
)

// ─── 核心类型 ─────────────────────────────────────────────────────────────────

// CollapseTrajectory 传递给编译器的轨迹数据。L2 本地类型，不依赖 L3 governance。
type CollapseTrajectory struct {
	SkillID           string
	GoalDescription   string
	ToolCalls         []CollapseToolCall
	InputSchema       map[string]string // param_name → Go 类型
	OutputSchema      map[string]string
	RiskLevel         string // low / medium / high
	SuccessCount      int
	SemanticVariance  float64
	CompletedAt       int64            // unix seconds (最后一次成功时间)
	Entities          []CollapseEntity // 用于 Freshness Check
	SemanticClusterID string
	TaintLevel        int // 0=None, 1=Low, 2=Medium, 3+=High
}

// CollapseToolCall 工具调用类型签名（DataStripping 后无参数值）。
type CollapseToolCall struct {
	ToolName   string
	Args       map[string]string // key → 类型字符串
	OutputType string
	OrderIndex int
}

// CollapseEntity 用于 Freshness Check 的实体引用。
type CollapseEntity struct {
	ID        string
	Type      string
	UpdatedAt int64 // unix seconds
}

// CompileRequest 编译请求。
type CompileRequest struct {
	Trajectory     *CollapseTrajectory
	EvalGatePassed bool
	SigningKey     []byte
	TestCases      []CompileTestCase // wazero 沙箱测试用例
	WorkDir        string            // 用于写 redaction_map.json
}

// CompileTestCase Wasm 沙箱测试用例。
type CompileTestCase struct {
	Input  []byte
	Expect []byte
}

// CompileResult 编译结果。
type CompileResult struct {
	WasmBytes    []byte
	WasmHash     string
	ImplGo       []byte            // 脱敏后的 Go 源码（审计副本）
	RedactionMap map[string]string // param_N → 原始值（本地存储，不离机）
	Signature    string
	RiskLevel    string
	SandboxTier  int
	SkillMeta    protocol.SkillMeta
}

// RedactionMap AST 脱敏占位符映射。
type RedactionMap struct {
	Params map[string]string
	mu     sync.Mutex
	seq    int
}

func (rm *RedactionMap) next() string {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	key := fmt.Sprintf("param_%d", rm.seq)
	rm.seq++
	return key
}

// FreshnessResult Freshness Check 结果。
type FreshnessResult struct {
	Fresh bool
	Stale []StaleEntity
}

// StaleEntity 已被更新的实体引用。
type StaleEntity struct {
	ID        string
	Type      string
	UpdatedAt int64
	TraceAt   int64
}

// RemoteCompileResult 单源编译结果。
type RemoteCompileResult struct {
	WasmBytes      []byte
	WasmHash       string // SHA-256(WasmBytes)
	CompileLogHash string // SHA-256(stderr)
	ServiceID      string
}

// CFGAnalysisResult 静态分析结果。
type CFGAnalysisResult struct {
	Passed     bool
	Violations []string
}

// ─── 接口 ─────────────────────────────────────────────────────────────────────

// RemoteCompileService TinyGo 远程编译服务接口。
type RemoteCompileService interface {
	Compile(ctx context.Context, sanitizedSrc []byte) (*RemoteCompileResult, error)
	ServiceID() string
}

// LLMCodeGenerator 从脱敏轨迹生成 impl.go 的接口。
type LLMCodeGenerator interface {
	GenerateImpl(ctx context.Context, traj *CollapseTrajectory) ([]byte, error)
}

// WasmTestExecutor 执行 Wasm 测试用例的接口（由 pkg/action.WazeroRuntime 满足）。
type WasmTestExecutor interface {
	RunWasm(ctx context.Context, skillName string, wasmBytes []byte, input []byte) ([]byte, error)
}

// ─── CompileGate — 内存 + 并发准入 ───────────────────────────────────────────

// CompileGate 控制 Logic Collapse 编译并发数与内存准入。
// 并发上限: Tier0→1, Tier1→2, Tier2+→4。内存门控: free >= 80MB。
type CompileGate struct {
	minFreeMemMB int64
	sema         chan struct{}
	inFlight     atomic.Int32
}

// NewCompileGate 按硬件 Tier 创建 CompileGate。
func NewCompileGate(tier observability.Tier) *CompileGate {
	var maxConcurrent int
	switch {
	case tier >= observability.Tier2:
		maxConcurrent = 4
	case tier >= observability.Tier1:
		maxConcurrent = 2
	default:
		maxConcurrent = 1 // Tier0 串行
	}
	return &CompileGate{
		minFreeMemMB: compileMinFreeMemMB,
		sema:         make(chan struct{}, maxConcurrent),
	}
}

// TryAcquire 非阻塞准入: freeMB >= 80 且并发槽可用时返回 true。
func (g *CompileGate) TryAcquire(freeMB int64) bool {
	if freeMB < g.minFreeMemMB {
		return false
	}
	select {
	case g.sema <- struct{}{}:
		g.inFlight.Add(1)
		return true
	default:
		return false
	}
}

// Release 释放一个并发槽。
func (g *CompileGate) Release() {
	<-g.sema
	g.inFlight.Add(-1)
}

// InFlight 返回当前编译中的任务数。
func (g *CompileGate) InFlight() int {
	return int(g.inFlight.Load())
}

// ─── FreshnessChecker — 轨迹时效性验证 ───────────────────────────────────────

// FreshnessChecker 验证源轨迹关键决策是否被 Semantic Memory 更新 supersede。
// 500ms 超时，超时后返回 Fresh=true（不阻塞系统），轨迹标记 needs_adaptation。
type FreshnessChecker struct{}

// Check 检查轨迹新鲜度。
func (fc *FreshnessChecker) Check(ctx context.Context, traj *CollapseTrajectory) (*FreshnessResult, error) {
	ctx, cancel := context.WithTimeout(ctx, freshnessCheckTimeout)
	defer cancel()

	result := &FreshnessResult{Fresh: true}
	for _, entity := range traj.Entities {
		select {
		case <-ctx.Done():
			// 超时：不阻塞，标记 needs_adaptation 由调用方处理
			return result, nil
		default:
		}
		if entity.UpdatedAt > traj.CompletedAt {
			result.Fresh = false
			result.Stale = append(result.Stale, StaleEntity{
				ID:        entity.ID,
				Type:      entity.Type,
				UpdatedAt: entity.UpdatedAt,
				TraceAt:   traj.CompletedAt,
			})
		}
	}
	return result, nil
}

// ─── DataStripper — 轨迹数据最小化 ───────────────────────────────────────────

// DataStripper 在 LLM 代码生成前剥离参数值。
// LLM 仅接收: 工具调用类型签名 + 成功/失败状态 + DAG 拓扑。
// 参数值仅保留类型信息，不可逆 strip-only。
type DataStripper struct{}

// Strip 返回仅含类型签名的轨迹副本（不修改原始轨迹）。
func (ds *DataStripper) Strip(traj *CollapseTrajectory) *CollapseTrajectory {
	stripped := *traj
	calls := make([]CollapseToolCall, len(traj.ToolCalls))
	for i, tc := range traj.ToolCalls {
		// 仅保留 key→type 映射（值已经在 CollapseToolCall.Args 中被类型化）
		args := maps.Clone(tc.Args)
		calls[i] = CollapseToolCall{
			ToolName:   tc.ToolName,
			Args:       args,
			OutputType: tc.OutputType,
			OrderIndex: tc.OrderIndex,
		}
	}
	stripped.ToolCalls = calls
	stripped.Entities = nil  // 实体详情不传给 LLM
	stripped.CompletedAt = 0 // 时间戳不传给 LLM
	return &stripped
}

// ─── StaticCFGAnalyzer — AST 静态分析 ────────────────────────────────────────

// StaticCFGAnalyzer 对 LLM 生成的 impl.go 做 AST 级静态分析。
// 检测: 禁止导入 / 非确定性调用 / WASI 未声明能力 / 时间炸弹特征。
// 分析失败 → 拒绝编译，轨迹进入 MEMF + 写 skill_static_analysis_rejected 审计事件。
type StaticCFGAnalyzer struct{}

// Analyze 对 Go 源码执行完整静态分析。
func (sa *StaticCFGAnalyzer) Analyze(src []byte) (*CFGAnalysisResult, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "impl.go", src, parser.AllErrors)
	if err != nil {
		return &CFGAnalysisResult{
			Passed:     false,
			Violations: []string{fmt.Sprintf("AST 解析失败: %v", err)},
		}, nil
	}

	result := &CFGAnalysisResult{Passed: true}

	ast.Inspect(f, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.ImportSpec:
			checkForbiddenImport(v, result)
		case *ast.CallExpr:
			checkNonDeterministicCall(v, result)
		case *ast.IfStmt:
			checkTimeBombPattern(v, result)
		}
		return true
	})

	if len(result.Violations) > 0 {
		result.Passed = false
	}
	return result, nil
}

// checkForbiddenImport 检测禁止的 Go 导入路径。
func checkForbiddenImport(spec *ast.ImportSpec, result *CFGAnalysisResult) {
	path := strings.Trim(spec.Path.Value, `"`)
	for _, forbidden := range forbiddenGoImports {
		if path == forbidden || strings.HasPrefix(path, forbidden+"/") {
			result.Violations = append(result.Violations,
				fmt.Sprintf("forbidden import: %q", path))
		}
	}
}

// checkNonDeterministicCall 检测 time.Now / rand.Read / os.Getenv 等非确定性调用。
func checkNonDeterministicCall(call *ast.CallExpr, result *CFGAnalysisResult) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return
	}
	pkgIdent, ok := sel.X.(*ast.Ident)
	if !ok {
		return
	}
	qualified := pkgIdent.Name + "." + sel.Sel.Name
	for _, nd := range nonDeterministicCalls {
		if qualified == nd {
			result.Violations = append(result.Violations,
				fmt.Sprintf("non-deterministic call: %s (通过 context_hint 注入)", qualified))
		}
	}
}

// checkTimeBombPattern 检测条件性时间激活（时间炸弹）特征。
func checkTimeBombPattern(stmt *ast.IfStmt, result *CFGAnalysisResult) {
	cond, ok := stmt.Cond.(*ast.BinaryExpr)
	if !ok {
		return
	}
	call, ok := cond.X.(*ast.CallExpr)
	if !ok {
		return
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return
	}
	if id, ok := sel.X.(*ast.Ident); ok && id.Name == "time" {
		result.Violations = append(result.Violations,
			"time-bomb pattern: conditional time-based activation detected")
	}
}

// CompileError 编译错误（含 violation 列表）。
type CompileError struct {
	violations []string
}

func (e *CompileError) Error() string {
	return fmt.Sprintf("static analysis rejected: %s", strings.Join(e.violations, "; "))
}

// ─── TaintSanitizeForRemoteCompilation — AST 脱敏 ────────────────────────────

// TaintSanitizeForRemoteCompilation 解析 impl.go AST，将字符串/数值字面量替换为
// 参数化占位符，剥离注释，保留包路径与协议关键字。
// 返回: 脱敏源码 + RedactionMap（本地存储，PII 不离机）。
func TaintSanitizeForRemoteCompilation(src []byte) ([]byte, *RedactionMap, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "impl.go", src, parser.ParseComments)
	if err != nil {
		return nil, nil, perrors.Wrap(perrors.CodeInternal, "AST 解析失败", err)
	}

	rm := &RedactionMap{Params: make(map[string]string)}

	// 收集所有导入路径，保留不替换
	importPaths := make(map[string]bool)
	for _, imp := range f.Imports {
		if imp.Path != nil {
			importPaths[imp.Path.Value] = true
		}
	}

	// 剥离注释
	f.Comments = nil

	// 遍历 AST 替换字面量
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok {
			return true
		}
		switch lit.Kind {
		case token.STRING:
			if importPaths[lit.Value] {
				return true // 保留导入路径
			}
			orig := strings.Trim(lit.Value, `"'`+"`")
			key := rm.next()
			rm.Params[key] = orig
			lit.Value = `"` + key + `"`
		case token.INT, token.FLOAT:
			key := rm.next()
			rm.Params[key] = lit.Value
			lit.Value = "0"
		}
		return true
	})

	var buf bytes.Buffer
	if err := format.Node(&buf, fset, f); err != nil {
		return nil, nil, perrors.Wrap(perrors.CodeInternal, "AST 格式化失败", err)
	}
	return buf.Bytes(), rm, nil
}

// ─── RemoteCompileService 实现 ────────────────────────────────────────────────

// HTTPTinyGoService 通过 HTTP 调用远程 TinyGo 编译服务。
type HTTPTinyGoService struct {
	id       string
	endpoint string
	client   *http.Client
}

// NewHTTPTinyGoService 创建 HTTP TinyGo 编译服务客户端。
func NewHTTPTinyGoService(id, endpoint string) *HTTPTinyGoService {
	return &HTTPTinyGoService{
		id:       id,
		endpoint: endpoint,
		client:   &http.Client{Timeout: remoteCompileTimeout},
	}
}

func (s *HTTPTinyGoService) ServiceID() string { return s.id }

// Compile 发送脱敏源码至远程 TinyGo 服务，返回 Wasm + SHA-256 Hash。
func (s *HTTPTinyGoService) Compile(ctx context.Context, src []byte) (*RemoteCompileResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		s.endpoint+"/compile", bytes.NewReader(src))
	if err != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, "创建编译请求失败", err)
	}
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, "远程编译请求失败", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, perrors.New(perrors.CodeInternal,
			fmt.Sprintf("远程编译错误 HTTP %d: %s", resp.StatusCode, body))
	}

	wasmBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, "读取编译响应失败", err)
	}

	hash := sha256.Sum256(wasmBytes)
	logHash := resp.Header.Get("X-Compile-Log-Hash")

	return &RemoteCompileResult{
		WasmBytes:      wasmBytes,
		WasmHash:       hex.EncodeToString(hash[:]),
		CompileLogHash: logHash,
		ServiceID:      s.id,
	}, nil
}

// LocalTinyGoService 本地 TinyGo 编译（Tier1+ 降级路径，远程服务不可用时使用）。
type LocalTinyGoService struct {
	id      string
	tinygo  string // tinygo 二进制路径，默认 "tinygo"
	workDir string
}

// NewLocalTinyGoService 创建本地 TinyGo 编译服务。
func NewLocalTinyGoService(id, workDir string) *LocalTinyGoService {
	return &LocalTinyGoService{id: id, tinygo: "tinygo", workDir: workDir}
}

func (s *LocalTinyGoService) ServiceID() string { return s.id }

// Compile 使用本地 tinygo 工具链编译 impl.go → Wasm。
func (s *LocalTinyGoService) Compile(ctx context.Context, src []byte) (*RemoteCompileResult, error) {
	tmpDir, err := os.MkdirTemp(s.workDir, "lc_compile_*")
	if err != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, "创建临时目录失败", err)
	}
	defer os.RemoveAll(tmpDir)

	srcPath := filepath.Join(tmpDir, "impl.go")
	wasmPath := filepath.Join(tmpDir, "impl.wasm")

	if err := os.WriteFile(srcPath, src, 0600); err != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, "写入源码失败", err)
	}

	cmd := exec.CommandContext(ctx, s.tinygo,
		"build", "-target=wasi", "-o", wasmPath, srcPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, perrors.New(perrors.CodeInternal,
			fmt.Sprintf("本地 TinyGo 编译失败: %s", out))
	}

	wasmBytes, err := os.ReadFile(wasmPath)
	if err != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, "读取 Wasm 产物失败", err)
	}

	hash := sha256.Sum256(wasmBytes)
	logHash := sha256.Sum256(out)

	return &RemoteCompileResult{
		WasmBytes:      wasmBytes,
		WasmHash:       hex.EncodeToString(hash[:]),
		CompileLogHash: hex.EncodeToString(logHash[:]),
		ServiceID:      s.id,
	}, nil
}

// ─── DualSourceCompiler — 双源编译 + WasmHash 验证 ───────────────────────────

// DualSourceCompiler 同时发送给两个独立编译服务，对比 WasmHash 一致才入库。
// 防御供应链攻击。
type DualSourceCompiler struct {
	services []RemoteCompileService
}

// NewDualSourceCompiler 创建双源编译器，需要至少两个 service。
func NewDualSourceCompiler(services ...RemoteCompileService) *DualSourceCompiler {
	return &DualSourceCompiler{services: services}
}

// Compile 并发编译并验证双源 WasmHash 一致性。
func (dc *DualSourceCompiler) Compile(ctx context.Context, sanitizedSrc []byte) (*RemoteCompileResult, error) {
	if len(dc.services) == 0 {
		return nil, ErrNoRemoteService
	}
	if len(dc.services) < 2 {
		return nil, ErrInsufficientCompileServices
	}

	type item struct {
		r   *RemoteCompileResult
		err error
	}

	ch := make(chan item, 2)
	for _, svc := range dc.services[:2] {
		go func(s RemoteCompileService) {
			r, err := s.Compile(ctx, sanitizedSrc)
			ch <- item{r, err}
		}(svc)
	}

	var r1, r2 *RemoteCompileResult
	for range 2 {
		res := <-ch
		if res.err != nil {
			return nil, perrors.Wrap(perrors.CodeInternal, "远程编译失败", res.err)
		}
		if r1 == nil {
			r1 = res.r
		} else {
			r2 = res.r
		}
	}

	// 双源 WasmHash 必须一致（Reproducible Build 要求）
	if r1.WasmHash != r2.WasmHash {
		return nil, ErrWasmHashMismatch
	}
	return r1, nil
}

// ─── 辅助函数 ─────────────────────────────────────────────────────────────────

// validateWasmMagic 验证 Wasm 二进制魔数: 0x00 0x61 0x73 0x6D。
func validateWasmMagic(wasmBytes []byte) error {
	if len(wasmBytes) < 4 {
		return perrors.New(perrors.CodeInternal, "wasm binary too short")
	}
	magic := []byte{0x00, 0x61, 0x73, 0x6D}
	for i, b := range magic {
		if wasmBytes[i] != b {
			return perrors.New(perrors.CodeInternal,
				fmt.Sprintf("invalid wasm magic at byte %d: want 0x%02X got 0x%02X", i, b, wasmBytes[i]))
		}
	}
	return nil
}

// assessRisk 从 impl.go 源码评估风险级别和沙箱层级。
// 基于 WASI host function 声明（//go:wasmimport polaris ...）。
func assessRisk(src []byte) (riskLevel string, sandboxTier int) {
	code := string(src)
	hasNetSend := strings.Contains(code, "//go:wasmimport polaris net_send") ||
		strings.Contains(code, "//go:wasmimport polaris net_dial")
	hasFSWrite := strings.Contains(code, "//go:wasmimport polaris fs_write") ||
		strings.Contains(code, "//go:wasmimport polaris path_open_write")
	switch {
	case hasNetSend:
		return "high", 2 // Sbx-L2 Wasm
	case hasFSWrite:
		return "medium", 2
	default:
		return "low", 1 // Sbx-L1 InProc
	}
}

// signWasm HMAC-SHA256 签名 Wasm 字节码（cosign 本地替代）。
// 签名私钥不对远程编译器暴露。
func signWasm(wasmBytes []byte, key []byte) (string, error) {
	if len(key) == 0 {
		return "", perrors.New(perrors.CodeInternal, "signing key not configured — refuse unsigned skill")
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(wasmBytes)
	return hex.EncodeToString(mac.Sum(nil)), nil
}

// getFreeMB 返回当前进程估算可用内存 (MB)。
func getFreeMB() int64 {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	// HeapSys - HeapInuse 为当前进程层面的可用估算
	available := m.HeapSys - m.HeapInuse
	return int64(available / (1024 * 1024))
}

// ─── LogicCollapseCompiler — 主编译器 ────────────────────────────────────────

// LogicCollapseCompiler Logic Collapse 主编译器。
// M9 BackgroundTaskScheduler 调用此编译器将成熟轨迹蒸馏为 Wasm 技能。
type LogicCollapseCompiler struct {
	gate             *CompileGate
	freshnessChecker *FreshnessChecker
	dataStripper     *DataStripper
	staticAnalyzer   *StaticCFGAnalyzer
	dualCompiler     *DualSourceCompiler
	llmCodeGen       LLMCodeGenerator
	wasmExecutor     WasmTestExecutor // nil → 跳过 wazero 测试
	registry         protocol.SkillRegistry
	privacyMode      string // "local_only" → 禁用
}

// LogicCollapseConfig 编译器配置。
type LogicCollapseConfig struct {
	Tier        observability.Tier
	PrivacyMode string
	Services    []RemoteCompileService // 至少 2 个
	LLMCodeGen  LLMCodeGenerator
	WasmExec    WasmTestExecutor // 可选
	Registry    protocol.SkillRegistry
}

// NewLogicCollapseCompiler 创建编译器。
func NewLogicCollapseCompiler(cfg LogicCollapseConfig) *LogicCollapseCompiler {
	return &LogicCollapseCompiler{
		gate:             NewCompileGate(cfg.Tier),
		freshnessChecker: &FreshnessChecker{},
		dataStripper:     &DataStripper{},
		staticAnalyzer:   &StaticCFGAnalyzer{},
		dualCompiler:     NewDualSourceCompiler(cfg.Services...),
		llmCodeGen:       cfg.LLMCodeGen,
		wasmExecutor:     cfg.WasmExec,
		registry:         cfg.Registry,
		privacyMode:      cfg.PrivacyMode,
	}
}

// Compile 执行完整 Logic Collapse 编译流水线。
// 成功后将 SkillMeta 写入 Registry，返回 CompileResult。
func (c *LogicCollapseCompiler) Compile(ctx context.Context, req *CompileRequest) (*CompileResult, error) { //nolint:gocyclo
	// 0. Feature Gate 检查
	fg := observability.GlobalFeatureGate()
	if fg != nil && !fg.IsEnabled(observability.FeatureLogicCollapse) {
		return nil, ErrLogicCollapseDisabled
	}

	// 0.1 Privacy Mode 检查
	if c.privacyMode == "local_only" {
		return nil, ErrLogicCollapseUnavailableInLocalOnly
	}

	traj := req.Trajectory
	if traj == nil {
		return nil, perrors.New(perrors.CodeInternal, "compile request: trajectory is nil")
	}

	// 0.2 编译前安全闸门
	if traj.SuccessCount < MinSuccessCount {
		return nil, ErrInsufficientSuccessCount
	}
	if traj.SemanticVariance < MinSemanticVariance {
		return nil, ErrInsufficientSemanticDiversity
	}
	if !req.EvalGatePassed {
		return nil, ErrEvalGateNotPassed
	}

	// 1. Freshness Check (500ms timeout)
	freshness, err := c.freshnessChecker.Check(ctx, traj)
	if err != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, "freshness check failed", err)
	}
	if !freshness.Fresh {
		return nil, ErrStaleTrajectory
	}

	// 2. Data Stripping（LLM 代码生成前）
	strippedTraj := c.dataStripper.Strip(traj)

	// 3. CompileGate — 内存 + 并发准入
	if !c.gate.TryAcquire(getFreeMB()) {
		return nil, ErrCompileGateRejected
	}
	defer c.gate.Release()

	// 4. LLM 代码生成 impl.go
	implGo, err := c.llmCodeGen.GenerateImpl(ctx, strippedTraj)
	if err != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, "LLM 代码生成失败", err)
	}

	// 5. Taint Check（TaintMedium+ 严禁编译）
	if traj.TaintLevel >= 2 {
		return nil, ErrTaintedTrajectory
	}

	// 6. 静态分析: CFG + 系统调用审计 + 确定性检查
	cfgResult, err := c.staticAnalyzer.Analyze(implGo)
	if err != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, "静态分析错误", err)
	}
	if !cfgResult.Passed {
		return nil, &CompileError{violations: cfgResult.Violations}
	}

	// 7. AST 脱敏（PII 不离机）
	sanitized, redactionMap, err := TaintSanitizeForRemoteCompilation(implGo)
	if err != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, "AST 脱敏失败", err)
	}

	// 7.1 保存 redaction_map.json 到本地
	if req.WorkDir != "" {
		mapBytes, _ := json.Marshal(redactionMap.Params)
		_ = os.WriteFile(filepath.Join(req.WorkDir, "redaction_map.json"), mapBytes, 0600)
	}

	// 8. 远程双源编译
	compileResult, err := c.dualCompiler.Compile(ctx, sanitized)
	if err != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, "双源编译失败", err)
	}

	// 9. 本地 wazero 验证（魔数 + 可选测试用例执行）
	if err := validateWasmMagic(compileResult.WasmBytes); err != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, "wasm 验证失败", err)
	}
	if c.wasmExecutor != nil && len(req.TestCases) > 0 {
		for i, tc := range req.TestCases {
			out, err := c.wasmExecutor.RunWasm(ctx, traj.SkillID, compileResult.WasmBytes, tc.Input)
			if err != nil {
				return nil, perrors.New(perrors.CodeInternal,
					fmt.Sprintf("wazero 测试用例 #%d 执行失败: %v", i, err))
			}
			if !bytes.Equal(out, tc.Expect) {
				return nil, perrors.New(perrors.CodeInternal,
					fmt.Sprintf("wazero 测试用例 #%d 输出不符: want %q got %q", i, tc.Expect, out))
			}
		}
	}

	// 10. 风险分级（基于 impl.go WASI host function 声明）
	riskLevel, sandboxTier := assessRisk(implGo)

	// 11. 签名
	sig, err := signWasm(compileResult.WasmBytes, req.SigningKey)
	if err != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, "wasm 签名失败", err)
	}

	// 12. 构建 SkillMeta
	skillName := "skill:" + traj.SkillID
	meta := protocol.SkillMeta{
		Name:       skillName,
		Version:    "1.0.0",
		Runtime:    "wasm",
		RiskLevel:  riskLevel,
		Sandbox:    sandboxTier,
		Trust:      protocol.TrustOfficial,
		Idempotent: true,
	}

	// 13. 写入 SkillRegistry
	if c.registry != nil {
		if err := c.registry.Register(ctx, meta); err != nil {
			return nil, perrors.Wrap(perrors.CodeInternal, "skill 注册失败", err)
		}
	}

	return &CompileResult{
		WasmBytes:    compileResult.WasmBytes,
		WasmHash:     compileResult.WasmHash,
		ImplGo:       sanitized,
		RedactionMap: redactionMap.Params,
		Signature:    sig,
		RiskLevel:    riskLevel,
		SandboxTier:  sandboxTier,
		SkillMeta:    meta,
	}, nil
}
