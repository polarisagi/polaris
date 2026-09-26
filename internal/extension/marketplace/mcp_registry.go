package marketplace

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// MCP Registry server.json（registry.modelcontextprotocol.io，MCP 官方发现标准）中的安装信息。
type mcpPackageDef struct {
	RegistryType         string       `json:"registryType"`
	Identifier           string       `json:"identifier"`
	Version              string       `json:"version"`
	RuntimeHint          string       `json:"runtimeHint"`
	Transport            mcpRemoteDef `json:"transport"`
	PackageArguments     []mcpArgDef  `json:"packageArguments"`
	EnvironmentVariables []mcpEnvDef  `json:"environmentVariables"`
}

type mcpArgDef struct {
	Type    string `json:"type"` // positional / named
	Name    string `json:"name"`
	Value   string `json:"value"`
	Default string `json:"default"`
}

type mcpEnvDef struct {
	Name       string `json:"name"`
	IsRequired bool   `json:"isRequired"`
	IsSecret   bool   `json:"isSecret"`
	Default    string `json:"default"`
}

// registryPageLimit / maxRegistryPages 全量同步分页；上限防止异常注册表让同步无止境。
const (
	registryPageLimit = 100
	maxRegistryPages  = 100
)

// ListRegistry 分页列出注册表中每个服务器的最新版本（catalog 同步用）。
func (c *MCPMarketplaceClient) ListRegistry(ctx context.Context, registryURL string) ([]protocol.RegistryEntry, error) {
	if registryURL == "" {
		registryURL = c.registryURL
	}
	var out []protocol.RegistryEntry
	cursor := ""
	for page := 0; page < maxRegistryPages; page++ {
		q := url.Values{"limit": {fmt.Sprint(registryPageLimit)}, "version": {"latest"}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		var resp struct {
			mcpRegistryResponse
			Metadata struct {
				NextCursor string `json:"nextCursor"`
			} `json:"metadata"`
		}
		if err := c.getJSON(ctx, registryURL+"/servers?"+q.Encode(), &resp); err != nil {
			return out, err
		}
		for _, s := range resp.Servers {
			if e, ok := registryEntry(s.Server); ok {
				out = append(out, e)
			}
		}
		if cursor = resp.Metadata.NextCursor; cursor == "" {
			return out, nil
		}
	}
	return out, nil
}

func (c *MCPMarketplaceClient) getJSON(ctx context.Context, u string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return apperr.Wrap(apperr.CodeInvalidInput, "marketplace: registry request", err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return apperr.Wrap(apperr.CodeNetworkUnavailable, "marketplace: registry", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return apperr.New(apperr.CodeNetworkUnavailable, "marketplace: registry returned "+resp.Status)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(v); err != nil {
		return apperr.Wrap(apperr.CodeInvalidInput, "marketplace: registry response", err)
	}
	return nil
}

// registryEntry server.json → 连接器目录条目：优先远程端点（无需本地运行时），否则取第一个
// 可在本机启动的包（npm→npx、pypi→uvx、oci→docker）。都不可用时不列出。
func registryEntry(s mcpServerDef) (protocol.RegistryEntry, bool) {
	e := protocol.RegistryEntry{ID: s.Name, Publisher: publisherFromName(s.Name), Type: "mcp",
		TrustTier: int(types.TrustCommunity), Name: s.Name, Description: s.Description, Homepage: s.Repository.URL,
		Version: s.Version, Timeout: 60}
	if len(s.Remotes) > 0 {
		e.Transport, e.URL = s.Remotes[0].Type, s.Remotes[0].URL
		return e, e.URL != ""
	}
	for _, p := range s.Packages {
		cmd, args, ok := packageCommand(p)
		if !ok {
			continue
		}
		e.Transport, e.Command, e.Args = "stdio", cmd, args
		if p.Transport.Type == "streamable-http" || p.Transport.Type == "sse" {
			e.Transport, e.URL = p.Transport.Type, p.Transport.URL
		}
		e.Env = map[string]string{}
		for _, env := range p.EnvironmentVariables {
			e.Env[env.Name] = env.Default
		}
		return e, true
	}
	return protocol.RegistryEntry{}, false
}

func packageCommand(p mcpPackageDef) (string, []string, bool) {
	var cmd string
	var args []string
	switch p.RegistryType {
	case "npm":
		cmd, args = "npx", []string{"-y", withVersion(p.Identifier, "@", p.Version)}
	case "pypi":
		cmd, args = "uvx", []string{withVersion(p.Identifier, "==", p.Version)}
	case "oci":
		cmd, args = "docker", []string{"run", "-i", "--rm", withVersion(p.Identifier, ":", p.Version)}
	default:
		return "", nil, false
	}
	for _, a := range p.PackageArguments {
		v := firstNonEmpty(a.Value, a.Default)
		if a.Type == "named" && a.Name != "" {
			args = append(args, a.Name)
		}
		if v != "" {
			args = append(args, v)
		}
	}
	return cmd, args, true
}

func withVersion(id, sep, version string) string {
	if version == "" || version == "latest" {
		return id
	}
	return id + sep + version
}
