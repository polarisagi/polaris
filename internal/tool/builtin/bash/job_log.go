package bash

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/google/uuid"

	"github.com/polarisagi/polaris/pkg/apperr"
)

const defaultMaxLogBytes = 50 * 1024 * 1024 // 50MB 默认上限（R2-16）

func validateJobID(jobID string) error {
	if jobID == "" {
		return apperr.New(apperr.CodeInvalidInput, "job_manager: job_id is required")
	}
	trimmed := strings.TrimPrefix(jobID, "job_")
	if _, err := uuid.Parse(trimmed); err != nil {
		return apperr.New(apperr.CodeInvalidInput, "job_manager: invalid job_id format, expected UUID")
	}
	return nil
}

type limitedLogWriter struct {
	file      *os.File
	maxBytes  int64
	written   int64
	truncated bool
	mu        sync.Mutex
}

func newLimitedLogWriter(file *os.File, maxBytes int64) *limitedLogWriter {
	if maxBytes <= 0 {
		maxBytes = defaultMaxLogBytes
	}
	return &limitedLogWriter{
		file:     file,
		maxBytes: maxBytes,
	}
}

func (w *limitedLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.truncated {
		return len(p), nil
	}

	remain := w.maxBytes - w.written
	if int64(len(p)) > remain {
		if remain > 0 {
			if n, wErr := w.file.Write(p[:remain]); wErr != nil {
				return n, apperr.Wrap(apperr.CodeInternal, "write log remain", wErr)
			}
			w.written += remain
		}
		marker := fmt.Sprintf("\n[LOG TRUNCATED: exceeded %d bytes limit]\n", w.maxBytes)
		if _, mErr := w.file.Write([]byte(marker)); mErr != nil {
			return len(p), apperr.Wrap(apperr.CodeInternal, "write log marker", mErr)
		}
		w.truncated = true
		return len(p), nil
	}

	n, err := w.file.Write(p)
	w.written += int64(n)
	if err != nil {
		return n, apperr.Wrap(apperr.CodeInternal, "write log", err)
	}
	return n, nil
}

func scanChunkForLines(buf []byte, n int, offset, size int64, foundLines, targetLines int) (int, int64, bool) {
	for i := n - 1; i >= 0; i-- {
		// 如果最后一位恰好是文件末尾的 \n，不计入行数计数
		if offset+int64(i) == size-1 && buf[i] == '\n' {
			continue
		}
		if buf[i] == '\n' {
			foundLines++
			if foundLines == targetLines {
				return foundLines, offset + int64(i) + 1, true
			}
		}
	}
	return foundLines, 0, false
}

// readTailLines 从文件尾部倒读获取最后 N 行（R2-16 替换 bufio.Scanner，消除 64KB 单行限制）。
func readTailLines(file *os.File, tailLines int) (string, error) {
	if tailLines <= 0 {
		tailLines = 200
	}
	stat, err := file.Stat()
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "stat log file", err)
	}
	size := stat.Size()
	if size == 0 {
		return "", nil
	}

	const chunkSize = 4096
	offset := size
	foundLines := 0
	buf := make([]byte, chunkSize)
	tailOffset := int64(0)

	for offset > 0 && foundLines < tailLines {
		readSize := int64(chunkSize)
		if offset < readSize {
			readSize = offset
		}
		offset -= readSize

		n, rErr := file.ReadAt(buf[:readSize], offset)
		if rErr != nil && !errors.Is(rErr, io.EOF) {
			return "", apperr.Wrap(apperr.CodeInternal, "read log chunk", rErr)
		}

		var matched bool
		foundLines, tailOffset, matched = scanChunkForLines(buf, n, offset, size, foundLines, tailLines)
		if matched {
			break
		}
	}

	data := make([]byte, size-tailOffset)
	if _, err := file.ReadAt(data, tailOffset); err != nil && !errors.Is(err, io.EOF) {
		return "", apperr.Wrap(apperr.CodeInternal, "read log tail", err)
	}
	return string(data), nil
}
