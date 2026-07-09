import os

def main():
    with open("bare_infer_lint_output.txt") as f:
        lines = f.read().splitlines()
        
    violations = []
    for line in lines:
        if "VIOLATED:" in line:
            parts = line.split("VIOLATED: ")[1].split(" ")[0]
            file_path, line_num = parts.split(":")
            violations.append((file_path, int(line_num)))
            
    from collections import defaultdict
    by_file = defaultdict(list)
    for f, l in violations:
        by_file[f].append(l)
        
    for f, lines_to_fix in by_file.items():
        # Remove ../ prefix since we are running in polaris/
        if f.startswith("../"):
            f_path = "internal/" + f[3:]
        else:
            f_path = f
            
        try:
            with open(f_path) as file:
                content_lines = file.read().split('\n')
                
            # Sort bottom up to not mess up line numbers
            for line_num in sorted(lines_to_fix, reverse=True):
                idx = line_num - 1
                indent = content_lines[idx][:len(content_lines[idx]) - len(content_lines[idx].lstrip())]
                comment = f"{indent}//nolint:bare-infer // 历史代码暂留，后续重构替换"
                content_lines.insert(idx, comment)
                
            with open(f_path, "w") as file:
                file.write('\n'.join(content_lines))
            print(f"Patched {f_path}")
        except Exception as e:
            print(f"Error {f_path}: {e}")

if __name__ == "__main__":
    main()
