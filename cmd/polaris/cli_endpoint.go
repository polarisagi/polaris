// CLI 客户端的服务发现与凭证注入。
//
// 解析顺序（ADR: docs/arch/decisions/ADR-0096-desktop-shell-and-daemon-client-split.md 决策五/六）：
//
//	① POLARIS_SERVER_URL —— 显式指定优先，配 POLARIS_API_KEY 作凭证（远程场景）
//	② run/ 运行时文件    —— 本机守护进程写入的实际端口与本地令牌
//	③ 兜底 localhost:28888 —— 守护进程未运行时给出可读的错误提示，而不是空地址
//
// 端口不再写死：守护进程可配置 port = 0 由内核分配，此时只有 run/polaris.port 知道
// 实际值（见 internal/runtimeinfo 包注释）。
package main

import (
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/runtimeinfo"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// cliTarget 是一次 CLI 调用的目标端点与凭证。
type cliTarget struct {
	BaseURL string
	Token   string
	// Err 记录发现过程中的失败原因，供 cliCheckServer 给出可操作的提示。
	// 不在发现阶段直接报错：`polaris help` 之类的命令根本不需要连服务。
	Err error
}

// cliEndpoint 惰性解析一次，进程内复用。
var cliEndpoint = sync.OnceValue(resolveCLITarget) //nolint:gochecknoglobals // 只读的惰性求值器，非可变全局状态

// resolveCLITarget 执行三段解析。
func resolveCLITarget() cliTarget {
	if u := os.Getenv("POLARIS_SERVER_URL"); u != "" {
		return cliTarget{BaseURL: strings.TrimRight(u, "/"), Token: os.Getenv("POLARIS_API_KEY")}
	}

	layout, err := cliDataLayout()
	if err != nil {
		return cliTarget{BaseURL: "http://localhost:28888", Err: err}
	}
	st, err := runtimeinfo.Read(runtimeinfo.Paths{
		PID:   layout.RunPID,
		Port:  layout.RunPort,
		Token: layout.RunToken,
	})
	if err != nil {
		// 区分"没运行"与"运行了但读不到凭证"：后者若被当成前者，用户会去反复
		// 重启一个其实活得好好的服务。
		return cliTarget{BaseURL: "http://localhost:28888", Err: err}
	}
	return cliTarget{
		BaseURL: "http://127.0.0.1:" + strconv.Itoa(st.Port),
		Token:   st.Token,
	}
}

// cliDataLayout 解析本机数据目录布局。配置读不到时退回默认根目录——
// CLI 是薄客户端，不该因为配置文件问题就完全不可用。
func cliDataLayout() (config.DataLayout, error) {
	var dirs config.DirsConfig
	if cfg, err := config.Load(configFilePath()); err == nil {
		dirs = cfg.System.Dirs
		if dataDir, derr := resolveDataDirBase(cfg); derr == nil {
			return config.NewDataLayout(dataDir, dirs), nil
		}
	}
	dataDir, err := resolveDataDirBase(nil)
	if err != nil {
		return config.DataLayout{}, err
	}
	return config.NewDataLayout(dataDir, dirs), nil
}

// cliServerURL 返回守护进程地址。
func cliServerURL() string { return cliEndpoint().BaseURL }

// cliAuthToken 返回本次调用使用的凭证，未发现时为空串。
func cliAuthToken() string { return cliEndpoint().Token }

// cliNewRequest 构造带凭证的请求。所有 CLI 出站请求都必须经此构造——
// 漏掉凭证的那一条会表现为"单个命令 401"，比整体不可用更难定位。
func cliNewRequest(method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequest(method, cliServerURL()+path, body)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, method+" "+path+" 构造请求失败", err)
	}
	if tok := cliAuthToken(); tok != "" {
		req.Header.Set("X-API-Key", tok)
	}
	return req, nil
}
