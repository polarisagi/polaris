package multi_edit

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/tool/builtin/guard"
	"github.com/polarisagi/polaris/pkg/apperr"
)

func MakeMultiEditFn(allowedPaths []string) sandbox.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		var args struct {
			Path  string `json:"path"`
			Edits []struct {
				OldStr string `json:"old_str"`
				NewStr string `json:"new_str"`
			} `json:"edits"`
		}
		if err := json.Unmarshal(input, &args); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "multi_edit: invalid args", err)
		}
		if err := guard.CheckAllowedPath(args.Path, allowedPaths); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "makeMultiEditFn", err)
		}
		cleanPath := filepath.Clean(args.Path)
		data, err := os.ReadFile(cleanPath)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "multi_edit: read failed", err)
		}
		original := string(data)

		// 第一遍：在原始内容中定位所有替换区间，防止链式污染。
		// 链式污染：顺序替换时 edit[0].NewStr 若包含 edit[1].OldStr，
		// 会被 edit[1] 二次替换，产生非预期结果。
		type region struct {
			start  int
			end    int
			newStr string
		}
		regions := make([]region, 0, len(args.Edits))
		for _, edit := range args.Edits {
			if strings.Count(original, edit.OldStr) != 1 {
				return nil, apperr.New(apperr.CodeInternal, fmt.Sprintf("multi_edit: old_str not unique or not found: %q", edit.OldStr))
			}
			idx := strings.Index(original, edit.OldStr)
			regions = append(regions, region{idx, idx + len(edit.OldStr), edit.NewStr})
		}

		// 按起始位置升序排列，便于重叠检测和顺序重建
		sort.Slice(regions, func(i, j int) bool { return regions[i].start < regions[j].start })

		// 检查区间重叠（两个 OldStr 在文件中位置交叉）
		for i := 1; i < len(regions); i++ {
			if regions[i].start < regions[i-1].end {
				return nil, apperr.New(apperr.CodeInternal, "multi_edit: edits overlap in file")
			}
		}

		// 从原始内容重建，避免任何链式副作用
		var buf strings.Builder
		cursor := 0
		for _, r := range regions {
			buf.WriteString(original[cursor:r.start])
			buf.WriteString(r.newStr)
			cursor = r.end
		}
		buf.WriteString(original[cursor:])

		if err := os.WriteFile(cleanPath, []byte(buf.String()), 0600); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "multi_edit: write failed", err)
		}
		return []byte(`{"status":"success"}`), nil
	}
}
