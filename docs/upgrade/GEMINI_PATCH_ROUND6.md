# GEMINI_PATCH_ROUND6

本轮修复四个经人工交叉核查确认为真实的缺陷，涉及四个文件。
每个问题给出精确定位、根因、以及完整修改方案。

---

## 问题一：RAG L2 断路 — buildPerceiveContext / buildPlanContext 缺失 SurrealDB 语义检索

**文件**: `pkg/cognition/kernel/memory_context.go`

**根因**: 两个函数只查 SQLite episodic/reflection（L1），Memory Agent `distill()` 写入 SurrealDB 的语义三元组（FTSSearch/VecKNN）在 Agent 构建上下文时从未被读回。

**修改方案**:

在 `memory_context.go` 顶部，紧跟已有 import 块之后，新增消费方接口（不引入新包依赖，接口定义在调用方）：

```go
// CognitiveSearcher L2 语义检索接口（消费方定义，防止包循环）。
// 实现由上层注入（pkg/cognition 层调用方提供 SurrealDBCoreStore）。
type CognitiveSearcher interface {
    // FTSSearch BM25 全文检索，返回 top-k 结果（docID + snippet）。
    FTSSearch(query string, k int) ([]CogResult, error)
    // VecKNN 向量近邻检索，embedding 为查询向量，k 为返回数量。
    VecKNN(embedding []float32, k int) ([]CogResult, error)
}

// CogResult 单条语义检索结果。
type CogResult struct {
    DocID   string
    Score   float64
    Snippet string
}
```

修改 `buildPerceiveContext` 函数签名，增加可选参数：

```go
// 修改前
func buildPerceiveContext(
    ctx context.Context,
    sCtx *protocol.StateContext,
    memory MemoryAccessor,
    pCtx *PromptContext,
) string

// 修改后
func buildPerceiveContext(
    ctx context.Context,
    sCtx *protocol.StateContext,
    memory MemoryAccessor,
    pCtx *PromptContext,
    cognitive CognitiveSearcher, // nil = 跳过 L2 检索，保持向后兼容
) string
```

在 `buildPerceiveContext` 函数体内，紧跟 episodic 查询结果的注入之后（约在 return 语句之前），新增 L2 语义检索段落：

```go
// ── L2 语义记忆（SurrealDB FTSSearch，Memory Agent 蒸馏写入）──────────────
if cognitive != nil && sCtx.Task.Goal != "" {
    ftsResults, err := cognitive.FTSSearch(sCtx.Task.Goal, 5)
    if err == nil && len(ftsResults) > 0 {
        pCtx.Builder.AddSection("语义记忆（L2）", func() string {
            var sb strings.Builder
            for _, r := range ftsResults {
                sb.WriteString(fmt.Sprintf("- [score=%.2f] %s\n", r.Score, r.Snippet))
            }
            return sb.String()
        }())
    }
}
```

同样修改 `buildPlanContext` 函数签名（同上，末尾加 `cognitive CognitiveSearcher`），并在其函数体的 return 前加相同的 L2 检索注入段落（query 改用 `sCtx.Task.Goal + " " + sCtx.Task.Type`）。

**调用方**（`state_machine.go`）中所有调用 `buildPerceiveContext`/`buildPlanContext` 的地方，将 `cognitive` 参数传入。`StateContext` 上游已有 `CognitiveStore` 字段（若无则新增 `Cognitive CognitiveSearcher`），如果注入为 nil（Tier-0 无 SurrealDB 连接时），函数静默跳过，不影响主流程。

---

## 问题二：Engine A 编译检查失效 — `patch_test.go` 被 go build 静默忽略

**文件**: `pkg/swarm/planner/pool.go`

**根因**: 第 127 行：
```go
testFile := filepath.Join(tmpDir, "patch_test.go")
```
Go 工具链对所有 `_test.go` 文件在 `go build` 阶段完全跳过。LLM 生成的代码无论包含任何语法错误，`buildErr` 永远为 nil，compileScore 永远跳过 0.0 分支，编译校验实际失效。

**修改方案**: 将文件名改为普通 Go 源文件：

```go
// 修改前（第 127 行）
testFile := filepath.Join(tmpDir, "patch_test.go")

// 修改后
testFile := filepath.Join(tmpDir, "patch_gen.go")
```

仅此一行改动。之后：
- `go build relDir` 会编译 `patch_gen.go`，真实捕获 LLM 生成代码的语法/类型错误
- `go test relDir` 仍然可以运行（如果生成代码包含 `TestXxx` 函数则由 `_test.go` 约定不强制）

注意：LLM prompt（第 101 行）中已要求 "Generate the Go code patch only"，生成代码应包含 `package` 声明。如果 LLM 返回不完整的代码段（缺少 package 声明），`go build` 会报 "expected 'package'" — 这是正确行为，说明生成质量不合格，score 应为 0.0。

---

## 问题三：SurpriseIndex 静态参数 — FastPath 永久锁死

**文件**: `pkg/cognition/kernel/agent.go`

**根因**: 第 221 行：
```go
a.sCtx.SurpriseIndex = observability.GlobalSurpriseIndex.ComputeBasic(
    context.Background(), nil, []string{"intent"})
```

两个硬编码参数导致收敛到 0：
1. `embedding = nil` → `computeCosineDist` 恒返回 `0.0`
2. `toolSeq = []string{"intent"}` → `"intent"` 在 `historicalTools` 中第二次出现后 jaccardDist 收敛到 `0.0`
3. 3 次调用后：`0.7*0.0 + 0.3*0.0 = 0.0` → `sCtx.SurpriseIndex = 0.0` → `agent_execute.go` 的 `> 0` 守卫永远失败

**修改方案**: 在调用处使用任务真实特征（task goal 关键词 + 最近工具历史）：

```go
// 修改前（第 221 行附近）
a.sCtx.SurpriseIndex = observability.GlobalSurpriseIndex.ComputeBasic(
    context.Background(), nil, []string{"intent"})

// 修改后
{
    // 提取 task goal 关键词作为 "工具序列" 的语义代理
    // 不同任务 → 不同关键词 → jaccardDist 有意义变化 → SurpriseIndex 真正反映任务新颖度
    goalWords := strings.Fields(a.sCtx.Task.Goal)
    if len(goalWords) > 8 {
        goalWords = goalWords[:8]
    }
    // 拼入 taskType 作为前缀，增加区分度
    toolSeq := append([]string{a.sCtx.Task.Type}, goalWords...)
    a.sCtx.SurpriseIndex = observability.GlobalSurpriseIndex.ComputeBasic(
        context.Background(),
        nil, // embedding 向量：待向量化流水线接入后替换；当前 cosineDist=0 可接受
        toolSeq,
    )
}
```

需要在文件顶部 import 中确认 `"strings"` 已导入。

此修改后：
- 新任务（首次出现的关键词）→ jaccardDist 趋近 1.0 → SurpriseIndex 约 0.3 → 触发 System 2
- 重复任务（关键词大量命中 historicalTools）→ jaccardDist 趋近 0 → SurpriseIndex < 0.3 → 触发 FastPath
- FastPath (`> 0 && < 0.3`) 现在有机会真正激活

---

## 问题四：MockProxy HTTPS CONNECT 隧道缺失 — 所有 HTTPS 请求协议层崩溃

**文件**: `pkg/action/mock_proxy.go`

**根因**: `ServeHTTP` 将 CONNECT 请求当作普通 HTTP 请求处理：哈希 "CONNECT host:443" 查表不中，返回 200 + JSON body。HTTP 客户端收到 200 后认为隧道已建立，随即发送 TLS ClientHello——但连接另一端读到的是 `{"mocked":true}` 而不是 TLS ServerHello，协议错误。

**修改方案**: 实现完整的 HTTPS MITM 拦截。新增以下内容到 `mock_proxy.go`：

### 1. 新增 import

```go
import (
    "context"
    "crypto/ecdsa"
    "crypto/elliptic"
    "crypto/rand"
    "crypto/sha256"
    "crypto/tls"
    "crypto/x509"
    "crypto/x509/pkix"
    "encoding/hex"
    "encoding/json"
    "encoding/pem"
    "math/big"
    "net"
    "net/http"
    "sync"
    "time"
)
```

### 2. 在 MockProxy 结构体中新增 CA 字段

```go
type MockProxy struct {
    mockTable map[string]MockResponse
    listener  net.Listener
    mu        sync.RWMutex
    server    *http.Server
    caKey     *ecdsa.PrivateKey  // 自签根 CA 私钥
    caCert    *x509.Certificate  // 根 CA 证书（DER 解析后）
    caCertPEM []byte             // 根 CA 证书 PEM（注入沙箱 SSL_CERT_FILE）
    caCertFile string            // 临时文件路径（Close 时删除）
}
```

### 3. 修改 NewMockProxy，在 Serve 前生成自签 CA

```go
func NewMockProxy(mockTable map[string]MockResponse) (*MockProxy, string, error) {
    ln, err := net.Listen("tcp", "127.0.0.1:0")
    if err != nil {
        return nil, "", err
    }
    mp := &MockProxy{mockTable: mockTable, listener: ln}

    // 生成自签根 CA（仅用于 DryRun 沙箱，生命周期与 MockProxy 相同）
    if err := mp.initCA(); err != nil {
        _ = ln.Close()
        return nil, "", err
    }

    // 将 CA cert 写到临时文件（供沙箱 SSL_CERT_FILE 指向）
    f, err := os.CreateTemp("", "polaris-mock-ca-*.pem")
    if err != nil {
        _ = ln.Close()
        return nil, "", err
    }
    if _, err := f.Write(mp.caCertPEM); err != nil {
        _ = ln.Close()
        return nil, "", err
    }
    _ = f.Close()
    mp.caCertFile = f.Name()

    mp.server = &http.Server{Handler: mp}
    go func() { _ = mp.server.Serve(ln) }()
    return mp, ln.Addr().String(), nil
}
```

### 4. 新增 initCA 方法

```go
func (mp *MockProxy) initCA() error {
    key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
    if err != nil {
        return err
    }
    tmpl := &x509.Certificate{
        SerialNumber:          big.NewInt(1),
        Subject:               pkix.Name{CommonName: "Polaris MockProxy CA"},
        NotBefore:             time.Now().Add(-time.Minute),
        NotAfter:              time.Now().Add(24 * time.Hour),
        IsCA:                  true,
        BasicConstraintsValid: true,
        KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
    }
    derBytes, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
    if err != nil {
        return err
    }
    cert, err := x509.ParseCertificate(derBytes)
    if err != nil {
        return err
    }
    mp.caKey = key
    mp.caCert = cert
    mp.caCertPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})
    return nil
}
```

### 5. 在 ServeHTTP 开头添加 CONNECT 分支

```go
func (mp *MockProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
    // HTTPS 隧道请求：劫持连接，MITM 解密后查 mockTable
    if r.Method == http.MethodConnect {
        mp.handleConnect(w, r)
        return
    }
    // 原有 HTTP mock 逻辑（保持不变）...
}
```

### 6. 新增 handleConnect 方法

```go
// handleConnect 处理 HTTP CONNECT 请求，实现 TLS MITM 拦截。
// 流程：响应 200 Connection established → 劫持 TCP 连接 → 用叶子证书做 TLS 握手
// → 以明文 HTTP 读取内部请求 → 查 mockTable → 响应。
func (mp *MockProxy) handleConnect(w http.ResponseWriter, r *http.Request) {
    host, _, err := net.SplitHostPort(r.Host)
    if err != nil {
        host = r.Host
    }

    // 为目标域名生成叶子证书（用根 CA 签名）
    leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
    if err != nil {
        http.Error(w, "cert gen failed", http.StatusInternalServerError)
        return
    }
    leafTmpl := &x509.Certificate{
        SerialNumber: big.NewInt(time.Now().UnixNano()),
        Subject:      pkix.Name{CommonName: host},
        DNSNames:     []string{host},
        NotBefore:    time.Now().Add(-time.Minute),
        NotAfter:     time.Now().Add(24 * time.Hour),
        KeyUsage:     x509.KeyUsageDigitalSignature,
        ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
    }
    leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, mp.caCert, &leafKey.PublicKey, mp.caKey)
    if err != nil {
        http.Error(w, "leaf cert failed", http.StatusInternalServerError)
        return
    }
    tlsCert := tls.Certificate{
        Certificate: [][]byte{leafDER},
        PrivateKey:  leafKey,
    }

    // 劫持底层 TCP 连接
    hj, ok := w.(http.Hijacker)
    if !ok {
        http.Error(w, "hijack not supported", http.StatusInternalServerError)
        return
    }
    conn, _, err := hj.Hijack()
    if err != nil {
        return
    }

    // 回应 CONNECT 200（标准 HTTP 代理协议要求）
    _, _ = conn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))

    // TLS 握手
    tlsConn := tls.Server(conn, &tls.Config{
        Certificates: []tls.Certificate{tlsCert},
        MinVersion:   tls.VersionTLS12,
    })
    defer tlsConn.Close()
    if err := tlsConn.Handshake(); err != nil {
        return
    }

    // 以明文读取内部 HTTP 请求，复用 ServeHTTP 逻辑
    innerReq, err := http.ReadRequest(bufio.NewReader(tlsConn))
    if err != nil {
        return
    }
    // 修正 URL（CONNECT 隧道内的请求 URL 是相对路径）
    if innerReq.URL.Host == "" {
        innerReq.URL.Host = r.Host
        innerReq.URL.Scheme = "https"
    }

    // 构造 ResponseWriter 写回 tlsConn
    rw := &connResponseWriter{conn: tlsConn}
    mp.ServeHTTP(rw, innerReq)
    _ = rw.flush()
}
```

### 7. 新增 connResponseWriter 辅助结构

```go
// connResponseWriter 将 http.ResponseWriter 接口桥接到裸 net.Conn。
type connResponseWriter struct {
    conn    net.Conn
    headers http.Header
    status  int
    body    []byte
}

func (c *connResponseWriter) Header() http.Header {
    if c.headers == nil {
        c.headers = make(http.Header)
    }
    return c.headers
}
func (c *connResponseWriter) WriteHeader(status int) { c.status = status }
func (c *connResponseWriter) Write(b []byte) (int, error) {
    c.body = append(c.body, b...)
    return len(b), nil
}
func (c *connResponseWriter) flush() error {
    if c.status == 0 {
        c.status = 200
    }
    resp := &http.Response{
        StatusCode: c.status,
        Proto:      "HTTP/1.1",
        ProtoMajor: 1, ProtoMinor: 1,
        Header:        c.headers,
        Body:          io.NopCloser(bytes.NewReader(c.body)),
        ContentLength: int64(len(c.body)),
    }
    return resp.Write(c.conn)
}
```

### 8. 修改 EnvVars，注入 CA 证书路径

```go
func (mp *MockProxy) EnvVars() map[string]string {
    addr := mp.listener.Addr().String()
    env := map[string]string{
        "HTTP_PROXY":  "http://" + addr,
        "HTTPS_PROXY": "http://" + addr,
        "NO_PROXY":    "",
    }
    if mp.caCertFile != "" {
        // Python requests、curl、Go net/http 均读取这两个环境变量
        env["SSL_CERT_FILE"]       = mp.caCertFile
        env["REQUESTS_CA_BUNDLE"]  = mp.caCertFile
        // Node.js
        env["NODE_EXTRA_CA_CERTS"] = mp.caCertFile
    }
    return env
}
```

### 9. 修改 Close，清理临时 CA 文件

```go
func (mp *MockProxy) Close() error {
    var errs []error
    if mp.server != nil {
        if err := mp.server.Shutdown(context.Background()); err != nil {
            errs = append(errs, err)
        }
    }
    if mp.caCertFile != "" {
        _ = os.Remove(mp.caCertFile)
    }
    if len(errs) > 0 {
        return errs[0]
    }
    return nil
}
```

### 10. 新增 import（文件顶部补全）

`handleConnect` 内部用到了 `bufio` 和 `bytes`、`io`，需确认顶部 import 包含：
```go
import (
    "bufio"
    "bytes"
    "context"
    "crypto/ecdsa"
    "crypto/elliptic"
    "crypto/rand"
    "crypto/sha256"
    "crypto/tls"
    "crypto/x509"
    "crypto/x509/pkix"
    "encoding/hex"
    "encoding/json"
    "encoding/pem"
    "io"
    "math/big"
    "net"
    "net/http"
    "os"
    "sync"
    "time"
)
```

---

## 编译与测试要求

完成以上修改后执行：

```bash
make fmt
make lint
make test
```

- `pool.go` 改动：执行 `go test ./pkg/swarm/planner/...` 确认无编译错误
- `mock_proxy.go` 改动：执行 `go test ./pkg/action/...` 确认无编译错误
- `agent.go` 改动：执行 `go test ./pkg/cognition/...` 确认无编译错误
- `memory_context.go` 改动：确认 `buildPerceiveContext` 和 `buildPlanContext` 的所有调用方都传入了 `cognitive` 参数（nil 可接受）

## 提交信息

```
fix(cognition): 接入 SurrealDB L2 语义检索到 perceive/plan 上下文组装
fix(planner): 修正 Engine A 生成文件命名，恢复编译检查有效性
fix(cognition): SurpriseIndex 改用任务关键词，解除 FastPath 永久锁死
fix(action): MockProxy 补全 HTTPS CONNECT MITM，注入 CA 证书到沙箱
```
