package downloader

import (
	"archive/tar"
	"archive/zip"
	"compress/bzip2"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// ExtractTarBz2 流式解压 .tar.bz2，对每个普通文件调用 mapper 决定是否写出及写入路径。
// mapper(tarEntryName) → (destAbsPath, shouldWrite)
func ExtractTarBz2(r io.Reader, mapper func(string) (string, bool)) error {
	bzr := bzip2.NewReader(r)
	tr := tar.NewReader(bzr)
	return extractTar(tr, mapper)
}

// ExtractTarGz 流式解压 .tar.gz。
func ExtractTarGz(r io.Reader, mapper func(string) (string, bool)) error {
	gzr, err := gzip.NewReader(r)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "downloader: gzip open failed", err)
	}
	defer gzr.Close()
	return extractTar(tar.NewReader(gzr), mapper)
}

func extractTar(tr *tar.Reader, mapper func(string) (string, bool)) error {
	written := 0
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, "downloader: tar read failed", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		destPath, ok := mapper(hdr.Name)
		if !ok {
			continue
		}
		if err := writeFromReader(tr, destPath, os.FileMode(hdr.Mode)|0o600); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "downloader: write "+destPath+" failed", err)
		}
		written++
	}
	if written == 0 {
		return apperr.New(apperr.CodeInternal, "downloader: no target files found in archive")
	}
	return nil
}

// ExtractZip 将 zipPath 内的条目提取到 destDir（扁平或保留相对路径，由 mapper 决定）。
// mapper(zipEntryName) → (destAbsPath, shouldWrite)；传 nil 则按相对路径全量提取。
func ExtractZip(zipPath, destDir string, mapper func(string) (string, bool)) error {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "downloader: open zip "+zipPath+" failed", err)
	}
	defer r.Close()

	written := 0
	for _, f := range r.File {
		if f.FileInfo().IsDir() {
			continue
		}
		var destPath string
		if mapper != nil {
			var ok bool
			destPath, ok = mapper(f.Name)
			if !ok {
				continue
			}
		} else {
			// 路径穿越防护：确保提取路径在 destDir 内
			rel := filepath.Clean(f.Name)
			if strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
				continue
			}
			destPath = filepath.Join(destDir, rel)
		}

		rc, err := f.Open()
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, "downloader: open zip entry "+f.Name+" failed", err)
		}
		writeErr := writeFromReader(rc, destPath, f.Mode()|0o600)
		rc.Close()
		if writeErr != nil {
			return apperr.Wrap(apperr.CodeInternal, "downloader: write "+destPath+" failed", writeErr)
		}
		written++
	}
	if written == 0 {
		return apperr.New(apperr.CodeInternal, "downloader: no files extracted from zip")
	}
	return nil
}

// writeFromReader 将 r 的内容原子写入 path（先写临时文件再 rename）。
func writeFromReader(r io.Reader, path string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "downloader: writeFromReader mkdir", err)
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "downloader: writeFromReader open", err)
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		os.Remove(tmp) //nolint:errcheck
		return apperr.Wrap(apperr.CodeInternal, "downloader: writeFromReader copy", err)
	}
	f.Close()
	return os.Rename(tmp, path)
}
