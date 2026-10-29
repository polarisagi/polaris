package lifecycle

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/polarisagi/polaris/internal/extension/pluginspec"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// CredentialCipher 敏感配置加解密（consumer-side；实现为 security/credential.Vault）。
type CredentialCipher interface {
	Encrypt(plaintext string) (string, error)
	Decrypt(ciphertext string) (string, error)
}

// PluginConfigService 插件 userConfig 的校验、存取与解析（ADR-0103 决策三）。
// 选项定义的权威源是 plugins.manifest 快照；本服务只管理用户填写的值。
type PluginConfigService struct {
	extRepo protocol.ExtensionRepository
	cipher  CredentialCipher // nil 时拒绝写入敏感值（fail-closed，不落明文）
}

func NewPluginConfigService(extRepo protocol.ExtensionRepository, cipher CredentialCipher) *PluginConfigService {
	return &PluginConfigService{extRepo: extRepo, cipher: cipher}
}

// ConfigScope 一组选项及其作用域（” 为插件级，否则为 channel 绑定的服务器名）。
type ConfigScope struct {
	Scope   string                        `json:"scope"`
	Title   string                        `json:"title,omitempty"`
	Options []pluginspec.UserConfigOption `json:"options"`
}

// ConfigValue API 视图：敏感值只报告是否已设置，绝不回传明文或密文。
type ConfigValue struct {
	Scope     string          `json:"scope"`
	Key       string          `json:"key"`
	Value     json.RawMessage `json:"value,omitempty"`
	Sensitive bool            `json:"sensitive"`
	IsSet     bool            `json:"is_set"`
}

// GetSchema 返回插件级与各 channel 的选项定义。
func (s *PluginConfigService) GetSchema(ctx context.Context, pluginID string) ([]ConfigScope, error) {
	plug, err := s.manifest(ctx, pluginID)
	if err != nil {
		return nil, err
	}
	scopes := []ConfigScope{{Scope: "", Options: plug.UserConfig}}
	for _, ch := range plug.Channels {
		scopes = append(scopes, ConfigScope{Scope: ch.Server, Title: ch.DisplayName, Options: ch.UserConfig})
	}
	return scopes, nil
}

// ListValues 返回已保存的值（敏感项脱敏）。
func (s *PluginConfigService) ListValues(ctx context.Context, pluginID string) ([]ConfigValue, error) {
	rows, err := s.extRepo.ListPluginUserConfig(ctx, pluginID)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeOf(err), "PluginConfigService.ListValues", err)
	}
	out := make([]ConfigValue, 0, len(rows))
	for _, r := range rows {
		v := ConfigValue{Scope: r.Scope, Key: r.Key, Sensitive: r.Sensitive, IsSet: r.Value != ""}
		if !r.Sensitive {
			v.Value = json.RawMessage(r.Value)
		}
		out = append(out, v)
	}
	return out, nil
}

// ConfigUpdate 单项更新。Value 为 JSON null 表示清除；敏感项未出现在更新集合中则保留原值
// （UI 不回传密钥，不能因为保存其他项而把已设置的 token 清空）。
type ConfigUpdate struct {
	Scope string          `json:"scope"`
	Key   string          `json:"key"`
	Value json.RawMessage `json:"value"`
}

// SaveValues 按选项定义校验并保存（整体替换，敏感项加密）。
func (s *PluginConfigService) SaveValues(ctx context.Context, pluginID string, updates []ConfigUpdate) error {
	schema, err := s.GetSchema(ctx, pluginID)
	if err != nil {
		return err
	}
	existing, err := s.extRepo.ListPluginUserConfig(ctx, pluginID)
	if err != nil {
		return apperr.Wrap(apperr.CodeOf(err), "PluginConfigService.SaveValues", err)
	}
	merged := map[string]types.PluginUserConfigRow{}
	for _, r := range existing {
		merged[r.Scope+"\x00"+r.Key] = r
	}
	for _, u := range updates {
		opt, ok := findOption(schema, u.Scope, u.Key)
		if !ok {
			return apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("unknown config option %q (scope %q)", u.Key, u.Scope))
		}
		k := u.Scope + "\x00" + u.Key
		if isJSONNull(u.Value) {
			delete(merged, k)
			continue
		}
		row, err := s.encodeValue(opt, u)
		if err != nil {
			return err
		}
		merged[k] = row
	}
	rows := make([]types.PluginUserConfigRow, 0, len(merged))
	for _, r := range merged {
		r.PluginID = pluginID
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Scope+rows[i].Key < rows[j].Scope+rows[j].Key })
	if err := s.extRepo.SavePluginUserConfig(ctx, pluginID, rows); err != nil {
		return apperr.Wrap(apperr.CodeOf(err), "PluginConfigService.SaveValues", err)
	}
	return nil
}

func (s *PluginConfigService) encodeValue(opt pluginspec.UserConfigOption, u ConfigUpdate) (types.PluginUserConfigRow, error) {
	if err := validateOptionValue(opt, u.Value); err != nil {
		return types.PluginUserConfigRow{}, err
	}
	row := types.PluginUserConfigRow{Scope: u.Scope, Key: u.Key, Value: string(u.Value), Sensitive: opt.Sensitive}
	if !opt.Sensitive {
		return row, nil
	}
	if s.cipher == nil {
		return row, apperr.New(apperr.CodeInternal, "credential vault unavailable; sensitive plugin config cannot be stored")
	}
	enc, err := s.cipher.Encrypt(string(u.Value))
	if err != nil {
		return row, apperr.Wrap(apperr.CodeInternal, "encrypt plugin config", err)
	}
	row.Value = enc
	return row, nil
}

// ResolveStrings 解析某作用域可用于 ${user_config.KEY} 替换的字符串值：插件级 + 该 channel 级
// （同名以 channel 级为准）+ 默认值。includeSensitive=false 时敏感值不出现在结果中
// （Claude：技能 / agent 正文里只替换非敏感值）。返回缺失的必填项。
func (s *PluginConfigService) ResolveStrings(ctx context.Context, pluginID, scope string, includeSensitive bool) (map[string]string, []string, error) {
	schema, err := s.GetSchema(ctx, pluginID)
	if err != nil {
		return nil, nil, err
	}
	rows, err := s.extRepo.ListPluginUserConfig(ctx, pluginID)
	if err != nil {
		return nil, nil, apperr.Wrap(apperr.CodeOf(err), "PluginConfigService.ResolveStrings", err)
	}
	saved := map[string]types.PluginUserConfigRow{}
	for _, r := range rows {
		saved[r.Scope+"\x00"+r.Key] = r
	}
	out := map[string]string{}
	var missing []string
	for _, sc := range schema {
		if sc.Scope != "" && sc.Scope != scope {
			continue
		}
		for _, opt := range sc.Options {
			val, ok, err := s.optionString(opt, saved[sc.Scope+"\x00"+opt.Key])
			if err != nil {
				return nil, nil, err
			}
			switch {
			case !ok && opt.Required:
				missing = append(missing, opt.Key)
			case ok && (!opt.Sensitive || includeSensitive):
				out[opt.Key] = val
			}
		}
	}
	return out, missing, nil
}

// optionString 已保存值优先，否则默认值；数组以 "," 连接（Claude multiple 选项）。
func (s *PluginConfigService) optionString(opt pluginspec.UserConfigOption, row types.PluginUserConfigRow) (string, bool, error) {
	raw := row.Value
	if raw != "" && row.Sensitive {
		if s.cipher == nil {
			return "", false, apperr.New(apperr.CodeInternal, "credential vault unavailable; cannot decrypt plugin config")
		}
		plain, err := s.cipher.Decrypt(raw)
		if err != nil {
			return "", false, apperr.Wrap(apperr.CodeInternal, "decrypt plugin config", err)
		}
		raw = plain
	}
	if raw == "" {
		if len(opt.Default) == 0 {
			return "", false, nil
		}
		raw = string(opt.Default)
	}
	return jsonScalarString(json.RawMessage(raw))
}

func jsonScalarString(raw json.RawMessage) (string, bool, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", false, apperr.Wrap(apperr.CodeInvalidInput, "plugin config value", err)
	}
	switch t := v.(type) {
	case nil:
		return "", false, nil
	case string:
		return t, t != "", nil
	case []any:
		parts := make([]string, 0, len(t))
		for _, item := range t {
			parts = append(parts, fmt.Sprint(item))
		}
		return strings.Join(parts, ","), len(parts) > 0, nil
	default:
		return fmt.Sprint(t), true, nil
	}
}

func (s *PluginConfigService) manifest(ctx context.Context, pluginID string) (*pluginspec.Plugin, error) {
	raw, err := s.extRepo.GetPluginManifest(ctx, pluginID)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeOf(err), "PluginConfigService.manifest", err)
	}
	var p pluginspec.Plugin
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "PluginConfigService: corrupt manifest snapshot", err)
	}
	return &p, nil
}

func findOption(schema []ConfigScope, scope, key string) (pluginspec.UserConfigOption, bool) {
	for _, sc := range schema {
		if sc.Scope != scope {
			continue
		}
		for _, o := range sc.Options {
			if o.Key == key {
				return o, true
			}
		}
	}
	return pluginspec.UserConfigOption{}, false
}

func isJSONNull(raw json.RawMessage) bool {
	return len(raw) == 0 || strings.TrimSpace(string(raw)) == "null"
}
