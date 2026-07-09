package guard

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/polarisagi/polaris/pkg/apperr"
)

func CheckForbiddenPath(path string) error {
	cleanPath := filepath.Clean(path)
	home, err := os.UserHomeDir()
	var forbidden []string
	if err == nil {
		forbidden = []string{
			filepath.Join(home, ".polarisagi", "polaris", "config"),
			filepath.Join(home, ".polarisagi", "polaris", "data"),
			filepath.Join(home, ".polarisagi", "polaris", "secrets"),
			filepath.Join(home, ".polarisagi", "polaris", "audit"),
			filepath.Join(home, ".ssh"),
			filepath.Join(home, ".gnupg"),
			"/etc",
			"/usr",
			"/bin",
			"/sbin",
		}
	} else {
		forbidden = []string{"/etc", "/usr", "/bin", "/sbin", "/root/.polarisagi"}
	}

	for _, f := range forbidden {
		if cleanPath == f || strings.HasPrefix(cleanPath, f+string(filepath.Separator)) {
			return apperr.New(apperr.CodeForbidden, fmt.Sprintf("write_file: path is in forbidden directory: %s", path))
		}
	}
	return nil
}

func CheckAllowedPath(path string, allowedPaths []string) error {
	if len(allowedPaths) == 0 {
		return apperr.New(apperr.CodeInternal, "path_guard: no allowed paths configured (fail-closed)")
	}
	clean := filepath.Clean(path)
	for _, allowed := range allowedPaths {
		allowedClean := filepath.Clean(allowed)
		if clean == allowedClean || strings.HasPrefix(clean, allowedClean+string(filepath.Separator)) {
			return nil
		}
	}
	return apperr.New(apperr.CodeInternal, fmt.Sprintf("path_guard: path %q not in allowed paths", path))
}

func IsPathAllowed(path string, allowedPaths []string) bool {
	if len(allowedPaths) == 0 {
		return false // fail-closed：空白名单拒绝所有
	}
	cleanPath := filepath.Clean(path)
	for _, allowed := range allowedPaths {
		cleanAllowed := filepath.Clean(allowed)
		// 精确匹配或严格子路径匹配（必须紧跟分隔符，防止前缀混淆）
		if cleanPath == cleanAllowed ||
			strings.HasPrefix(cleanPath, cleanAllowed+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func GetTodoPath(allowedPaths []string) (string, error) {
	if len(allowedPaths) == 0 {
		return "", apperr.New(apperr.CodeInternal, "todo: no workspace configured")
	}
	return filepath.Join(allowedPaths[0], ".polaris_todo.json"), nil
}
