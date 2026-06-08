package sysenv

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// SystemInfo 包含系统的静态及动态上下文信息
type SystemInfo struct {
	OSName        string `json:"os_name"`
	OSVersion     string `json:"os_version"`
	Architecture  string `json:"architecture"`
	Locale        string `json:"locale"`
	Timezone      string `json:"timezone"`
	CPUCores      int    `json:"cpu_cores"`
	Username      string `json:"username"`
	IsRoot        bool   `json:"is_root"`
	HomeDir       string `json:"home_dir"`
	MemoryTotalGB string `json:"memory_total_gb"`
	MemoryUsageGB string `json:"memory_usage_gb"`
	DiskFreeGB    string `json:"disk_free_gb"`
}

// GetSystemInfo 获取系统信息
func GetSystemInfo() *SystemInfo {
	info := &SystemInfo{
		OSName:       runtime.GOOS,
		Architecture: runtime.GOARCH,
		CPUCores:     runtime.NumCPU(),
	}

	// 时区
	info.Timezone, _ = time.Now().Zone()

	// 语言及编码
	info.Locale = os.Getenv("LANG")
	if info.Locale == "" {
		info.Locale = os.Getenv("LC_ALL")
	}
	if info.Locale == "" {
		info.Locale = "unknown"
	}

	// 用户信息
	u, err := user.Current()
	if err != nil {
		info.Username = "unknown"
		info.HomeDir = "unknown"
	} else {
		info.Username = u.Username
		info.HomeDir = u.HomeDir
		uid, uidErr := strconv.Atoi(u.Uid)
		if uidErr == nil && uid == 0 {
			info.IsRoot = true
		} else if strings.EqualFold(u.Username, "root") || strings.EqualFold(u.Username, "Administrator") {
			info.IsRoot = true
		}
	}

	// 磁盘空间
	var stat syscall.Statfs_t
	if err := syscall.Statfs("/", &stat); err == nil {
		// Bavail 是非超级用户可用的块数，Bsize 是块大小
		// 部分系统使用 Bsize，部分使用 Frsize，通常对于 macOS/Linux Bsize * Bavail 适用
		// 避免 lint 报错 unnecessary conversion，stat.Bavail 本身可能已经是 uint64，但不同 OS 下不同
		freeBytes := stat.Bavail * uint64(stat.Bsize)
		info.DiskFreeGB = fmt.Sprintf("%.2f", float64(freeBytes)/(1024*1024*1024))
	} else {
		info.DiskFreeGB = "unknown"
	}

	// OS 版本和内存信息 (因平台而异)
	info.OSVersion = getOSVersion()
	info.MemoryTotalGB, info.MemoryUsageGB = getMemoryInfo()

	return info
}

// FormatMarkdown 将系统信息格式化为 Markdown 字符串，便于注入 System Prompt
func (i *SystemInfo) FormatMarkdown() string {
	var sb strings.Builder
	sb.WriteString("### System Environment\n")
	sb.WriteString(fmt.Sprintf("- **OS**: %s %s (%s)\n", i.OSName, i.OSVersion, i.Architecture))
	sb.WriteString(fmt.Sprintf("- **CPU**: %d cores\n", i.CPUCores))
	sb.WriteString(fmt.Sprintf("- **Memory**: %s GB used / %s GB total\n", i.MemoryUsageGB, i.MemoryTotalGB))
	sb.WriteString(fmt.Sprintf("- **Disk (/)**: %s GB free\n", i.DiskFreeGB))
	rootStatus := ""
	if i.IsRoot {
		rootStatus = " (Root/Admin privileges)"
	}
	sb.WriteString(fmt.Sprintf("- **User**: %s%s, Home: %s\n", i.Username, rootStatus, i.HomeDir))
	sb.WriteString(fmt.Sprintf("- **Locale**: %s\n", i.Locale))
	sb.WriteString(fmt.Sprintf("- **Timezone**: %s\n", i.Timezone))
	return sb.String()
}

func getOSVersion() string {
	switch runtime.GOOS {
	case "darwin":
		out, err := exec.Command("sw_vers", "-productVersion").Output()
		if err == nil {
			return strings.TrimSpace(string(out))
		}
	case "linux":
		// 尝试读取 /etc/os-release 中的 PRETTY_NAME
		if data, err := os.ReadFile("/etc/os-release"); err == nil {
			lines := strings.Split(string(data), "\n")
			for _, line := range lines {
				if strings.HasPrefix(line, "PRETTY_NAME=") {
					val := strings.TrimPrefix(line, "PRETTY_NAME=")
					return strings.Trim(val, `"'`)
				}
			}
		}
		// 降级尝试
		out, err := exec.Command("uname", "-r").Output()
		if err == nil {
			return strings.TrimSpace(string(out))
		}
	case "windows":
		out, err := exec.Command("cmd", "/c", "ver").Output()
		if err == nil {
			return strings.TrimSpace(string(out))
		}
	}
	return "unknown"
}

func getMemoryInfo() (totalGB, usageGB string) {
	switch runtime.GOOS {
	case "darwin":
		return getMemoryInfoDarwin()
	case "linux":
		return getMemoryInfoLinux()
	default:
		return "unknown", "unknown"
	}
}

func getMemoryInfoDarwin() (totalGB, usageGB string) {
	totalGB = "unknown"
	usageGB = "unknown"

	// Mac 获取总内存
	outTotal, err := exec.Command("sysctl", "-n", "hw.memsize").Output()
	if err == nil {
		if memSize, err := strconv.ParseUint(strings.TrimSpace(string(outTotal)), 10, 64); err == nil {
			totalGB = fmt.Sprintf("%.2f", float64(memSize)/(1024*1024*1024))
		}
	}

	// Mac 获取已用内存 (通过 vm_stat)
	outVm, err := exec.Command("vm_stat").Output()
	if err == nil {
		lines := strings.Split(string(outVm), "\n")
		var activePages, wiredPages uint64
		for _, line := range lines {
			parts := strings.Split(line, ":")
			if len(parts) != 2 {
				continue
			}
			key := strings.TrimSpace(parts[0])
			valStr := strings.TrimSpace(strings.TrimRight(parts[1], "."))
			val, _ := strconv.ParseUint(valStr, 10, 64)
			switch key {
			case "Pages active":
				activePages = val
			case "Pages wired down":
				wiredPages = val
			}
		}
		usedBytes := (activePages + wiredPages) * 4096
		usageGB = fmt.Sprintf("%.2f", float64(usedBytes)/(1024*1024*1024))
	}
	return totalGB, usageGB
}

func getMemoryInfoLinux() (totalGB, usageGB string) {
	totalGB = "unknown"
	usageGB = "unknown"

	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return totalGB, usageGB
	}

	lines := strings.Split(string(data), "\n")
	var memTotal, memFree, buffers, cached uint64
	for _, line := range lines {
		parts := strings.Fields(line)
		if len(parts) >= 2 {
			val, _ := strconv.ParseUint(parts[1], 10, 64)
			switch parts[0] {
			case "MemTotal:":
				memTotal = val
			case "MemFree:":
				memFree = val
			case "Buffers:":
				buffers = val
			case "Cached:":
				cached = val
			}
		}
	}
	if memTotal > 0 {
		totalGB = fmt.Sprintf("%.2f", float64(memTotal)/(1024*1024)) // proc 中单位是 KB
		usedKB := memTotal - memFree - buffers - cached
		usageGB = fmt.Sprintf("%.2f", float64(usedKB)/(1024*1024))
	}
	return totalGB, usageGB
}
