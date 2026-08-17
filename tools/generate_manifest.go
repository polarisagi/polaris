//go:build ignore

// generate_manifest 重算不可变内核包的 SHA-256 清单，写入 internal/config/kernel_manifest.json。
// 该清单由 internal/config/integrity_check.go 以 //go:embed 内嵌，在启动 §0.5 做 fail-fast 校验。
//
// -check 模式（2026-08-17 新增）：只比对不写回，drift 时退出非零。
//
// 为什么必须有 -check：本工具此前只挂在 Makefile 的 build* 目标下，于是
//  1. `make build` 会静默重写一个受版本控制的文件（工作区无故变脏）；
//  2. 只改内核源码而不 build 的提交，清单就永久停在旧哈希上，而 check-all 全程看不见。
//
// 2026-08-17 实测这条缺口已经炸了：清单停在 2026-08-13 的 cb7ca5d，此后 14 个内核
// 文件哈希变更、3 个新文件未登记、1 条指向已删除文件，`go run ./cmd/polaris` 在
// §0.5 直接拒绝启动。生成物与源的一致性只能靠门控，不能靠"记得跑一下 build"——
// 与 docs-gen-check 同理。
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/polarisagi/polaris/internal/config"
)

func main() {
	checkOnly := flag.Bool("check", false, "只校验清单与源码是否一致，drift 时退出非零，不写回")
	flag.Parse()

	manifest := make(map[string]string)
	for _, dir := range config.ImmutableKernelPackages() {
		err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if !info.IsDir() && filepath.Ext(path) == ".go" {
				f, err := os.Open(path)
				if err != nil {
					return err
				}
				defer f.Close()
				h := sha256.New()
				if _, err := io.Copy(h, f); err != nil {
					return err
				}
				manifest[path] = hex.EncodeToString(h.Sum(nil))
			}
			return nil
		})
		if err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "Error walking %s: %v\n", dir, err)
			os.Exit(1)
		}
	}

	b, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error marshaling manifest: %v\n", err)
		os.Exit(1)
	}

	outPath := "internal/config/kernel_manifest.json"

	if *checkOnly {
		current, err := os.ReadFile(outPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "manifest-check: 读取 %s 失败: %v\n", outPath, err)
			os.Exit(2)
		}
		// json.MarshalIndent 不带尾换行；按写入端的字节形态比对，避免"格式差异"被误判为 drift。
		if bytes.Equal(bytes.TrimRight(current, "\n"), bytes.TrimRight(b, "\n")) {
			fmt.Println("manifest-check ok")
			return
		}
		var committed map[string]string
		if err := json.Unmarshal(current, &committed); err != nil {
			fmt.Fprintf(os.Stderr, "manifest-check: %s 不是合法 JSON: %v\n", outPath, err)
			os.Exit(1)
		}
		for path, want := range committed {
			got, ok := manifest[path]
			if !ok {
				fmt.Fprintf(os.Stderr, "%s: 清单登记了已不存在的文件\n", path)
				continue
			}
			if got != want {
				fmt.Fprintf(os.Stderr, "%s: 哈希漂移（清单 %s，实际 %s）\n", path, want[:12], got[:12])
			}
		}
		for path := range manifest {
			if _, ok := committed[path]; !ok {
				fmt.Fprintf(os.Stderr, "%s: 内核包新增文件未登记进清单\n", path)
			}
		}
		fmt.Fprintf(os.Stderr, "FAIL: %s 与内核源码不一致——启动 §0.5 完整性校验会拒绝启动。"+
			"跑 `make generate-manifest` 重新生成并提交。\n", outPath)
		os.Exit(1)
	}

	if err := os.WriteFile(outPath, b, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "Error writing manifest: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Generated %s\n", outPath)
}
