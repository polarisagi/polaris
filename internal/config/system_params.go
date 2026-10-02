package config

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// ============================================================================
// 系统全局参数与规范名称的单一事实源（Single Source of Truth, SSoT）
// 所有目录、默认文件名、环境变量名、服务标识均在此定义，禁止在业务代码中散落硬编码字符串。
// ============================================================================

const (
	// AppName 系统应用名称。
	AppName = "polaris"

	// ServiceLabelLaunchd macOS launchd 守护进程服务标识（ADR-0096 反向域名标识）。
	ServiceLabelLaunchd = "com.polarisagi.polaris"

	// WindowsTaskName Windows 任务计划程序中的服务标识名。
	WindowsTaskName = "PolarisAGI-Polaris"
)

// 全局统一环境变量名定义。
const (
	// EnvPolarisDataDir 指定运行时数据根目录（最高优先级，覆盖配置文件）。
	EnvPolarisDataDir = "POLARIS_DATA_DIR"

	// EnvPolarisConfig 指定配置文件绝对或相对路径。
	EnvPolarisConfig = "POLARIS_CONFIG"

	// EnvPolarisAPIKey 全局 API Key 鉴权环境变量。
	EnvPolarisAPIKey = "POLARIS_API_KEY"

	// EnvPolarisVaultPassphrase 保密凭据库冷启动密钥环境变量。
	EnvPolarisVaultPassphrase = "POLARIS_VAULT_PASSPHRASE"

	// EnvPolarisFeatureGate 硬件门控覆盖环境变量。
	EnvPolarisFeatureGate = "POLARIS_FEATURE_GATE"

	// EnvPolarisTier 硬件分级覆盖环境变量。
	EnvPolarisTier = "POLARIS_TIER"
)

// 核心目录名称与相对路径定义。
const (
	// DefaultDataDirRel 默认数据根目录相对于用户家目录的相对路径。
	// 若需更改系统默认目录，仅需修改此处的常量值。
	DefaultDataDirRel = ".polaris"

	// DefaultDataDirTilde 默认数据根目录的 ~ 表示法（用于配置文件与文档）。
	DefaultDataDirTilde = "~/" + DefaultDataDirRel

	// 规范子目录名
	SubdirData       = "data"
	SubdirLogs       = "logs"
	SubdirConfig     = "config"
	SubdirExtensions = "extensions"
	SubdirSkills     = "skills"
	SubdirModels     = "models"
	SubdirWorkspace  = "workspace"
	SubdirSessions   = "sessions"
	SubdirAudit      = "audit"
	SubdirReports    = "reports"
	SubdirCache      = "cache"
	SubdirHooks      = "hooks"
	SubdirTmp        = "tmp"
	SubdirBin        = "bin"
	SubdirRun        = "run"
	SubdirEval       = "eval"
	SubdirSecrets    = "secrets"
	SubdirPrompts    = "prompts"
	SubdirArchive    = "archive"
)

// 规范文件名定义。
const (
	ConfigFileName         = "config.toml"
	SQLiteDBFileName       = "polaris.db"
	SurrealDBFileName      = "surreal.db"
	KillSwitchFileName     = "KILLSWITCH"
	FullStopFileName       = ".fullstop"
	VaultKeyFileName       = "vault.key"
	PIDFileName            = "polaris.pid"
	PortFileName           = "polaris.port"
	TokenFileName          = "polaris.token"
	LockFileName           = "polaris.lock"
	CLISessionFileName     = "cli_session.json"
	CLILastSessionFileName = "last_cli_session"
	AllowlistFileName      = "local_only_network_allowlist.toml"
	SkillSignKeyFileName   = "skill_signing.key"
	SoulMDFileName         = "SOUL.md"

	// CLIScanBufferMaxBytes 本地交互式 CLI 单行输入缓冲区上限（64 KiB）。
	CLIScanBufferMaxBytes = 64 * 1024
)

// DefaultDataDir 返回当前用户家目录下的默认规范数据根目录（如 ~/.polaris）。
func DefaultDataDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "failed to resolve user home directory", err)
	}
	return filepath.Join(home, DefaultDataDirRel), nil
}

// ExpandHome 展开路径开头的 ~/ 为用户绝对家目录路径。若不以 ~/ 开头则原样返回。
func ExpandHome(p string) string {
	if len(p) >= 2 && p[:2] == "~/" {
		home, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

// ResolveDataDir 统一解析运行时数据根目录的权威决议函数：
// 1. 优先读取 POLARIS_DATA_DIR 环境变量；
// 2. 其次读取传入的 explicitDir（如配置文件或 CLI 参数中的 system.data_dir）；
// 3. 兜底使用默认目录 DefaultDataDir()；
// 4. 对结果进行 ~/ 展开并执行平滑迁移逻辑。
func ResolveDataDir(explicitDir string) (string, error) {
	dir := os.Getenv(EnvPolarisDataDir)
	if dir == "" {
		dir = explicitDir
	}
	if dir == "" || dir == DefaultDataDirTilde {
		var err error
		dir, err = DefaultDataDir()
		if err != nil {
			return "", err
		}
	} else if strings.HasPrefix(dir, "~/") {
		dir = ExpandHome(dir)
	}
	return dir, nil
}

// ResolveConfigFile 统一解析配置文件绝对路径：
// 1. 优先读取 POLARIS_CONFIG 环境变量；
// 2. 其次使用 explicitPath（CLI -config 传入）；
// 3. 再次在 dataDir 下查找 config.toml；
// 4. 兜底回退到默认数据目录下的 config.toml。
func ResolveConfigFile(explicitPath, dataDir string) string {
	if envPath := os.Getenv(EnvPolarisConfig); envPath != "" {
		return ExpandHome(envPath)
	}
	if explicitPath != "" {
		return ExpandHome(explicitPath)
	}
	if dataDir != "" {
		cfgPath := filepath.Join(dataDir, ConfigFileName)
		if _, err := os.Stat(cfgPath); err == nil {
			return cfgPath
		}
	}

	// 默认路径检查
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, DefaultDataDirRel, ConfigFileName)
	}
	return filepath.Join(".", ConfigFileName)
}
