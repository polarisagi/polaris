// Package read_file 实现内置工具 read_file：按行读取文本文件（ADR-0120 决策三第 5 条）。
//
// 输出带行号、支持 offset/limit 读取片段；未指定范围时最多返回 defaultMaxLines 行 / defaultMaxBytes 字节，
// 并注明总行数。此前只能整篇读取：大文件一次进上下文，按未命中价付清之后，每次调用还都要携带它。
package read_file

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/tool/builtin/guard"
	"github.com/polarisagi/polaris/pkg/apperr"
)

const (
	defaultMaxLines = 2000
	defaultMaxBytes = 64 << 10
	// maxLineBytes 单行上限：超长行（压缩 JS、单行 JSON）截断，避免一行吃掉全部预算。
	maxLineBytes = 4000
)

type readFileArgs struct {
	Path   string `json:"path"`
	Offset int    `json:"offset"`
	Limit  int    `json:"limit"`
}

func MakeReadFileFn(allowedPaths []string) sandbox.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		// ADR-0097 决策五：项目会话追加项目工作目录为可访问根（会话级，不改进程级白名单）。
		paths := guard.ScopedPaths(ctx, allowedPaths)
		var args readFileArgs
		if err := json.Unmarshal(input, &args); err != nil {
			return nil, apperr.Wrap(apperr.CodeInvalidInput, "read_file: invalid args", err)
		}
		if err := guard.CheckReadablePath(ctx, args.Path, paths); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "makeReadFileFn", err)
		}
		f, err := os.Open(filepath.Clean(args.Path))
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "read_file", err)
		}
		defer f.Close()
		return readLines(f, args.Offset, args.Limit)
	}
}

// nextLine 读一行（去掉行尾换行），超长行只保留前 maxLineBytes+1 字节，其余丢弃。
func nextLine(rd *bufio.Reader) ([]byte, error) {
	var buf []byte
	got := false
	for {
		frag, isPrefix, err := rd.ReadLine()
		if err != nil {
			if got {
				return buf, nil
			}
			if errors.Is(err, io.EOF) {
				return nil, io.EOF
			}
			return nil, apperr.Wrap(apperr.CodeInternal, "read_file: read line", err)
		}
		got = true
		if room := maxLineBytes + 1 - len(buf); room > 0 {
			if len(frag) > room {
				frag = frag[:room]
			}
			buf = append(buf, frag...)
		}
		if !isPrefix {
			return buf, nil
		}
	}
}

func writeNumbered(out *bytes.Buffer, n int, b []byte) {
	if len(b) > maxLineBytes {
		b = append(b[:maxLineBytes:maxLineBytes], []byte("…[line truncated]")...)
	}
	out.WriteString(strconv.Itoa(n))
	out.WriteByte('\t')
	out.Write(b)
	out.WriteByte('\n')
}

// lineWindow 记录一次读取的窗口与产出状态。
type lineWindow struct {
	offset, limit     int
	explicit          bool
	line, first, last int
	truncated         bool
	out               bytes.Buffer
}

// take 处理第 line 行；返回 error 仅当首行是二进制。
func (w *lineWindow) take(b []byte) error {
	w.line++
	if w.line < w.offset || w.truncated {
		return nil
	}
	if w.line >= w.offset+w.limit || (!w.explicit && w.out.Len() >= defaultMaxBytes) {
		w.truncated = true
		return nil
	}
	if w.line == 1 && bytes.IndexByte(b, 0) >= 0 {
		return apperr.New(apperr.CodeInvalidInput, "read_file: binary file; inspect it with bash (file, xxd, strings)")
	}
	if w.first == 0 {
		w.first = w.line
	}
	w.last = w.line
	writeNumbered(&w.out, w.line, b)
	return nil
}

func (w *lineWindow) footer() {
	switch {
	case w.first == 0 && w.line > 0:
		fmt.Fprintf(&w.out, "[offset %d is past the end; the file has %d lines]", w.offset, w.line)
	case w.truncated || (w.first > 1):
		fmt.Fprintf(&w.out, "[showing lines %d-%d of %d; use offset/limit]", w.first, w.last, w.line)
	}
}

// readLines 输出 "<行号>\t<内容>"；offset 为 1 起始的首行，limit 为行数（<=0 用默认上限）。
func readLines(f *os.File, offset, limit int) ([]byte, error) {
	if offset <= 0 {
		offset = 1
	}
	w := &lineWindow{offset: offset, limit: limit, explicit: limit > 0}
	if !w.explicit {
		w.limit = defaultMaxLines
	}
	rd := bufio.NewReaderSize(f, 64<<10)
	for {
		b, err := nextLine(rd)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if err := w.take(b); err != nil {
			return nil, err
		}
	}
	w.footer()
	return w.out.Bytes(), nil
}
