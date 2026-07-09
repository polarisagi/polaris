package connector

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/polarisagi/polaris/pkg/apperr"

	"github.com/fsnotify/fsnotify"

	"github.com/polarisagi/polaris/pkg/concurrent"
	"github.com/polarisagi/polaris/pkg/types"
)

// ObsidianConnector 是一个 P0 级的本地 Markdown 连接器，符合 M10 规范。
// 监听本地的一个文件夹 (默认作为透明记忆目录，允许人工修改)。
var _ KnowledgeSourceConnector = (*ObsidianConnector)(nil)

type ObsidianConnector struct {
	basePath string
	exclude  []string
	watcher  *fsnotify.Watcher
}

// NewObsidianConnector 创建一个新的 Obsidian/Local Folder 连接器。
func NewObsidianConnector(basePath string) (*ObsidianConnector, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "NewObsidianConnector", err)
	}

	c := &ObsidianConnector{
		basePath: basePath,
		exclude:  []string{".obsidian", ".trash", ".git"},
		watcher:  watcher,
	}

	return c, nil
}

func (c *ObsidianConnector) ID() string {
	return "obsidian-local-connector"
}

func (c *ObsidianConnector) Name() string {
	return "Obsidian Local Vault Connector"
}

func (c *ObsidianConnector) SyncConfig() types.SyncConfig {
	return types.SyncConfig{
		DefaultInterval: 300,  // 5 分钟的默认全量比对，作为 watch 失败的兜底
		SupportsWatch:   true, // 核心：开启 fsnotify
		MaxBatchSize:    100,
	}
}

func (c *ObsidianConnector) isExcluded(path string) bool {
	// 判断是否被隐藏目录包裹
	parts := strings.Split(path, string(os.PathSeparator))
	for _, part := range parts {
		for _, ex := range c.exclude {
			if part == ex {
				return true
			}
		}
	}
	return false
}

// List 会递归扫描 basePath，生成当前所有的 Markdown 文件的 DocumentRef。
func (c *ObsidianConnector) List(ctx context.Context) ([]*types.DocumentRef, error) {
	var refs []*types.DocumentRef

	err := filepath.WalkDir(c.basePath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, "ObsidianConnector.List", err)
		}

		if d.IsDir() {
			if c.isExcluded(path) {
				return filepath.SkipDir
			}
			return nil
		}

		if c.isExcluded(path) {
			return nil
		}

		if filepath.Ext(path) != ".md" {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return nil //nolint:nilerr
		}

		// 计算 hash
		data, err := os.ReadFile(path)
		if err != nil {
			return nil //nolint:nilerr
		}
		hash := sha256.Sum256(data)
		hashStr := hex.EncodeToString(hash[:])

		relPath, _ := filepath.Rel(c.basePath, path)

		refs = append(refs, &types.DocumentRef{
			URI:         "file://" + relPath,
			Title:       strings.TrimSuffix(filepath.Base(path), ".md"),
			SourceType:  "markdown",
			ContentHash: hashStr,
			ModifiedAt:  info.ModTime().Unix(),
			Size:        info.Size(),
		})

		return nil
	})

	if err != nil {
		return refs, apperr.Wrap(apperr.CodeInternal, "ObsidianConnector.List", err)
	}
	return refs, nil
}

// Fetch 读取单个文件的内容，并分离出 YAML frontmatter 作为元数据。
func (c *ObsidianConnector) Fetch(ctx context.Context, ref *types.DocumentRef) (*types.SyncDocument, error) {
	relPath := filepath.Clean(strings.TrimPrefix(ref.URI, "file://"))
	fullPath := filepath.Join(c.basePath, relPath)

	base := filepath.Clean(c.basePath)
	if !strings.HasPrefix(fullPath, base+string(os.PathSeparator)) && fullPath != base {
		return nil, apperr.New(apperr.CodeForbidden, "path traversal detected")
	}

	data, err := os.ReadFile(fullPath)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "ObsidianConnector.Fetch", err)
	}

	contentStr := string(data)
	meta := make(map[string]string)

	// 简单解析 yaml frontmatter (例如 "---" 包裹的区域)
	if strings.HasPrefix(contentStr, "---\n") || strings.HasPrefix(contentStr, "---\r\n") { //nolint:nestif
		parts := strings.SplitN(contentStr, "---", 3)
		if len(parts) >= 3 {
			// frontmatter = parts[1]
			// content = parts[2]
			lines := strings.Split(parts[1], "\n")
			for _, line := range lines {
				line = strings.TrimSpace(line)
				if line == "" {
					continue
				}
				kv := strings.SplitN(line, ":", 2)
				if len(kv) == 2 {
					meta[strings.TrimSpace(kv[0])] = strings.TrimSpace(kv[1])
				}
			}
			contentStr = strings.TrimSpace(parts[2])
		}
	}

	return &types.SyncDocument{
		URI:      ref.URI,
		Title:    ref.Title,
		Content:  []byte(contentStr),
		Metadata: meta,
	}, nil
}

// Watch 启动基于 fsnotify 的文件系统监听，并将事件桥接到 M10 的流水线。
func (c *ObsidianConnector) Watch(ctx context.Context) (<-chan types.ChangeEvent, error) { //nolint:gocyclo
	out := make(chan types.ChangeEvent, 100)

	// 添加基础路径和子目录
	err := filepath.WalkDir(c.basePath, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			if c.isExcluded(path) {
				return filepath.SkipDir
			}
			_ = c.watcher.Add(path)
		}
		return nil
	})
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "ObsidianConnector.Watch", err)
	}

	concurrent.SafeGo(ctx, "knowledge.connector.obsidian_watch", func(ctx context.Context) {
		defer c.watcher.Close()
		defer close(out)

		for {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-c.watcher.Events:
				if !ok {
					return
				}

				if c.isExcluded(event.Name) || filepath.Ext(event.Name) != ".md" {
					continue
				}

				// 对于 fsnotify.Create, 若是目录，需要加入 watcher
				if event.Op&fsnotify.Create == fsnotify.Create {
					info, err := os.Stat(event.Name)
					if err == nil && info.IsDir() {
						_ = c.watcher.Add(event.Name)
						continue
					}
				}

				var evType string
				if event.Op&fsnotify.Create == fsnotify.Create {
					evType = "created"
				} else if event.Op&fsnotify.Write == fsnotify.Write {
					evType = "updated"
				} else if event.Op&fsnotify.Remove == fsnotify.Remove || event.Op&fsnotify.Rename == fsnotify.Rename {
					evType = "deleted"
				} else {
					continue
				}

				relPath, _ := filepath.Rel(c.basePath, event.Name)
				uri := "file://" + relPath

				var hashStr string
				var modTime int64
				var size int64

				if evType != "deleted" {
					data, err := os.ReadFile(event.Name)
					if err == nil {
						hash := sha256.Sum256(data)
						hashStr = hex.EncodeToString(hash[:])
						info, err := os.Stat(event.Name)
						if err == nil {
							modTime = info.ModTime().Unix()
							size = info.Size()
						}
					} else {
						// 读取失败可能由于被抢占或者锁，等待 M10 同步重试
						continue
					}
				}

				docRef := &types.DocumentRef{
					URI:         uri,
					Title:       strings.TrimSuffix(filepath.Base(event.Name), ".md"),
					SourceType:  "markdown",
					ContentHash: hashStr,
					ModifiedAt:  modTime,
					Size:        size,
				}

				out <- types.ChangeEvent{
					Type: evType,
					Ref:  docRef,
				}
			case <-c.watcher.Errors:
				// just continue on watch errors
			}
		}
	})

	return out, nil
}
