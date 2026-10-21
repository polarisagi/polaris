package policy

import (
	"embed"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/polarisagi/polaris/internal/config"
)

// 敏感路径目录（ADR-0122 决策二）：路径分类的单一事实源。
// Decide、文件工具 guard、沙箱 DenyPaths/DenyWritePaths、原文扫描片段都从这里取，
// 新增敏感软件只改 pathcatalog/*.yaml，禁止在代码里散写路径。

//go:embed pathcatalog/*.yaml
var pathCatalogFS embed.FS

// catalogEntry 目录文件的一条记录。
type catalogEntry struct {
	Class     string   `yaml:"class"`
	Paths     []string `yaml:"paths"`
	Patterns  []string `yaml:"patterns"`  // basename 类：文件名通配
	Anchors   []string `yaml:"anchors"`   // persistence 类：任意位置的相对路径锚点
	Fragments []string `yaml:"fragments"` // 原文扫描特征片段
	Note      string   `yaml:"note"`
}

type catalogFile struct {
	entries []catalogEntry
	err     error
}

// loadCatalogFiles 解析一次内嵌目录文件（编译期常量，进程内不变，故可惰性缓存）。
var loadCatalogFiles = sync.OnceValue(func() map[string]catalogFile { //nolint:gochecknoglobals // 惰性解析编译期内嵌的只读文件，无可变状态
	out := make(map[string]catalogFile, 4)
	for _, name := range []string{"all", "darwin", "linux", "windows"} {
		raw, err := pathCatalogFS.ReadFile("pathcatalog/" + name + ".yaml")
		if err != nil {
			out[name] = catalogFile{err: err}
			continue
		}
		var es []catalogEntry
		err = yaml.Unmarshal(raw, &es)
		out[name] = catalogFile{entries: es, err: err}
	}
	return out
})

// PathClass 路径分类结果。Persistence/System 仅在 write=true 时置位（读它们无害）；
// Basename 仅在路径不在可信根内时置位。
type PathClass struct {
	Self, Credential, Persistence, System, Basename bool
}

// Blocked 命中任一需要确认/拒绝的非 Self 类别（调用方按读写口径取 ClassifyPath 的 write 参数即可）。
func (pc PathClass) Blocked() bool {
	return pc.Credential || pc.Persistence || pc.System || pc.Basename
}

// reason 对应 Decide 硬限制原因码；凭据类优先于持久化、持久化优先于系统目录。
func (pc PathClass) reason() string {
	switch {
	case pc.Credential || pc.Basename:
		return ReasonSensitivePath
	case pc.Persistence:
		return ReasonPersistencePath
	case pc.System:
		return ReasonSystemPath
	}
	return ""
}

func reasonRank(r string) int {
	switch r {
	case ReasonSensitivePath:
		return 3
	case ReasonPersistencePath:
		return 2
	case ReasonSystemPath:
		return 1
	}
	return 0
}

// strongerReason 取两个路径原因码中更严的一个。
func strongerReason(a, b string) string {
	if reasonRank(b) > reasonRank(a) {
		return b
	}
	return a
}

type catPattern struct {
	raw  string   // 展开后的具体路径（可含 * 通配段）
	segs []string // 逐段（已按平台折叠大小写）
}

// PathCatalog 展开后的目录实例（绑定 home/env/goos）。构造有少量 stat 开销，
// 热路径应复用实例，不要逐路径重建。
type PathCatalog struct {
	goos      string
	fold      bool
	invalid   bool
	self      []catPattern
	selfExec  []catPattern
	cred      []catPattern
	pers      []catPattern
	sys       []catPattern
	anchors   [][]string
	basenames []string
	fragments []string
}

var envVarRe = regexp.MustCompile(`%([^%]+)%`)

// NewPathCatalog 按当前进程环境构造目录。
func NewPathCatalog() *PathCatalog {
	home, _ := os.UserHomeDir()
	return newPathCatalog(runtime.GOOS, home, os.Getenv, selfLayouts())
}

// newPathCatalogHome 使用调用方给定的家目录（Decide 的 Context.Home）。
func newPathCatalogHome(home string) *PathCatalog {
	return newPathCatalog(runtime.GOOS, home, os.Getenv, selfLayouts())
}

func newPathCatalog(goos, home string, getenv func(string) string, layouts []config.DataLayout) *PathCatalog {
	c := &PathCatalog{goos: goos, fold: goos == "darwin" || goos == "windows"}
	files := loadCatalogFiles()
	names := []string{"all"}
	switch goos {
	case "darwin", "linux", "windows":
		names = append(names, goos)
	}
	realHome := ""
	if home != "" {
		realHome = resolveReal(home)
	}
	for _, n := range names {
		f := files[n]
		if f.err != nil {
			c.invalid = true // 内嵌文件损坏：fail-closed，Classify 一律视为敏感
			return c
		}
		for _, e := range f.entries {
			c.addEntry(e, home, realHome, getenv)
		}
	}
	for _, p := range selfPathsOf(layouts) {
		c.self = append(c.self, c.pattern(p))
	}
	for _, p := range selfExecPathsOf(layouts) {
		cp := c.pattern(p)
		c.selfExec = append(c.selfExec, cp)
		c.pers = append(c.pers, cp)
	}
	c.fragments = append(c.fragments, selfFragments(layouts)...)
	c.fragments = dedupeStrings(c.fragments)
	return c
}

func (c *PathCatalog) addEntry(e catalogEntry, home, realHome string, getenv func(string) string) {
	c.fragments = append(c.fragments, e.Fragments...)
	switch e.Class {
	case "basename":
		for _, p := range e.Patterns {
			if c.fold {
				p = strings.ToLower(p)
			}
			c.basenames = append(c.basenames, p)
		}
	case "credential", "persistence", "system":
		for _, raw := range e.Paths {
			for _, exp := range c.expand(raw, home, realHome, getenv) {
				cp := c.pattern(exp)
				switch e.Class {
				case "credential":
					c.cred = append(c.cred, cp)
				case "persistence":
					c.pers = append(c.pers, cp)
				default:
					c.sys = append(c.sys, cp)
				}
			}
		}
		for _, a := range e.Anchors {
			c.anchors = append(c.anchors, c.segs(a))
		}
	default:
		c.invalid = true // 未知类别视为目录文件损坏
	}
}

// expand 展开 ~ 与 %VAR%；变量未设置返回 nil（跳过该条）。已存在的路径追加其真实路径变体
// （/etc → /private/etc），家目录另取真实路径变体，避免逐条 EvalSymlinks 的开销。
func (c *PathCatalog) expand(p, home, realHome string, getenv func(string) string) []string {
	var bases []string
	if strings.HasPrefix(p, "~") {
		if home == "" {
			return nil
		}
		rest := p[1:]
		bases = append(bases, strings.TrimRight(home, `/\`)+rest)
		if realHome != "" && realHome != home {
			bases = append(bases, strings.TrimRight(realHome, `/\`)+rest)
		}
	} else {
		bases = []string{p}
	}
	var out []string
	for _, b := range bases {
		missing := false
		b = envVarRe.ReplaceAllStringFunc(b, func(m string) string {
			v := getenv(m[1 : len(m)-1])
			if v == "" {
				missing = true
			}
			return strings.TrimRight(v, `/\`)
		})
		if missing {
			continue
		}
		out = append(out, b)
		if c.goos == runtime.GOOS && !strings.ContainsAny(b, "*?[") {
			if _, err := os.Lstat(b); err == nil {
				if r, err := filepath.EvalSymlinks(b); err == nil && r != b {
					out = append(out, r)
				}
			}
		}
	}
	return out
}

func (c *PathCatalog) pattern(p string) catPattern {
	return catPattern{raw: p, segs: c.segs(p)}
}

// segs 路径 → 逐段切片。Windows 统一分隔符；darwin/windows 折叠大小写（APFS/NTFS 默认不敏感）。
func (c *PathCatalog) segs(p string) []string {
	if c.goos == "windows" {
		p = strings.ReplaceAll(p, `\`, "/")
	}
	p = path.Clean(p)
	if c.fold {
		p = strings.ToLower(p)
	}
	if p == "/" {
		return []string{""}
	}
	return strings.Split(p, "/")
}

func segEq(pat, s string) bool {
	if !strings.ContainsAny(pat, "*?[") {
		return pat == s
	}
	ok, err := path.Match(pat, s)
	return err == nil && ok
}

// hasPrefixSegs pat 是 cand 的逐段前缀（精确或其子路径；逐段比较杜绝 /Library 命中 ~/Library）。
func hasPrefixSegs(pat, cand []string) bool {
	if len(pat) == 0 || len(cand) < len(pat) {
		return false
	}
	for i := range pat {
		if !segEq(pat[i], cand[i]) {
			return false
		}
	}
	return true
}

func matchAny(pats []catPattern, cand []string) bool {
	for _, p := range pats {
		if hasPrefixSegs(p.segs, cand) {
			return true
		}
	}
	return false
}

// containsSegs anchor 在 cand 任意位置出现（作为连续段序列的前缀起点）。
func containsSegs(anchor, cand []string) bool {
	for i := 0; i+len(anchor) <= len(cand); i++ {
		if hasPrefixSegs(anchor, cand[i:]) {
			return true
		}
	}
	return false
}

// Classify 判定 p 的类别。原路径与真实路径（含不存在尾部的最长存在前缀解析）任一命中即算。
func (c *PathCatalog) Classify(p string, write bool, roots []string) PathClass {
	if c.invalid {
		return PathClass{Credential: true}
	}
	p = filepath.Clean(p)
	cands := []string{p}
	if r := resolveReal(p); r != p {
		cands = append(cands, r)
	}
	var rootSegs [][]string
	for _, r := range roots {
		if r == "" {
			continue
		}
		rootSegs = append(rootSegs, c.segs(filepath.Clean(r)), c.segs(resolveReal(r)))
	}
	var pc PathClass
	for _, cand := range cands {
		c.classifyCand(&pc, c.segs(cand), write, rootSegs)
	}
	return pc
}

// classifyCand 对一条候选路径（原路径或真实路径）累加分类标志。
func (c *PathCatalog) classifyCand(pc *PathClass, s []string, write bool, rootSegs [][]string) {
	pc.Self = pc.Self || matchAny(c.self, s)
	pc.Credential = pc.Credential || matchAny(c.cred, s)
	if write {
		pc.Persistence = pc.Persistence || matchAny(c.pers, s) || c.anchored(s)
		pc.System = pc.System || matchAny(c.sys, s)
	}
	if len(c.basenames) > 0 && !inRootSegs(s, rootSegs) {
		base := s[len(s)-1]
		for _, b := range c.basenames {
			if segEq(b, base) {
				pc.Basename = true
			}
		}
	}
}

func (c *PathCatalog) anchored(s []string) bool {
	for _, a := range c.anchors {
		if containsSegs(a, s) {
			return true
		}
	}
	return false
}

func inRootSegs(s []string, roots [][]string) bool {
	for _, r := range roots {
		if hasPrefixSegs(r, s) {
			return true
		}
	}
	return false
}

// denyPaths 沙箱清单：rw = self ∪ credential；writeOnly = persistence ∪ system（不含 anchors）。
// 通配段展开到已存在的路径；无通配的条目原样保留（不存在的目录将来被创建时同样受保护）。
func (c *PathCatalog) denyPaths() (rw, writeOnly []string) {
	rw = expandGlobs(c.self, c.cred)
	writeOnly = expandGlobs(c.pers, c.sys)
	return rw, writeOnly
}

func expandGlobs(groups ...[]catPattern) []string {
	seen := make(map[string]struct{})
	var out []string
	add := func(p string) {
		if _, ok := seen[p]; ok {
			return
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	for _, g := range groups {
		for _, p := range g {
			if !strings.ContainsAny(p.raw, "*?[") {
				add(p.raw)
				continue
			}
			matches, err := filepath.Glob(p.raw)
			if err != nil {
				continue
			}
			for _, m := range matches {
				add(m)
			}
		}
	}
	return out
}

func dedupeStrings(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := in[:0:0]
	for _, s := range in {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// selfFragments 自保护路径相对数据根目录名的片段（如 ".polaris/vault.key"），供原文扫描。
func selfFragments(layouts []config.DataLayout) []string {
	var out []string
	for _, l := range layouts {
		if l.Root == "" {
			continue
		}
		root := filepath.Base(l.Root)
		for _, p := range []string{l.VaultKeyFile, l.ConfigFile, l.KillSwitchFile, l.FullStopFile,
			l.CLISessionFile, l.Secrets, l.Data, l.Audit, l.Eval, l.Config} {
			rel, err := filepath.Rel(l.Root, p)
			if err != nil || strings.HasPrefix(rel, "..") {
				continue
			}
			out = append(out, filepath.ToSlash(filepath.Join(root, rel)))
		}
	}
	return out
}

// ClassifyPath 判定绝对路径 p 的类别（按当前进程环境）。Basename 仅在 p 不在 roots 内时置位。
func ClassifyPath(p string, write bool, roots []string) PathClass {
	return NewPathCatalog().Classify(p, write, roots)
}

// SandboxDenyPaths 沙箱路径清单：rw 读写都拒（self ∪ credential），writeOnly 只拒写（persistence ∪ system）。
func SandboxDenyPaths() (rw []string, writeOnly []string) {
	return NewPathCatalog().denyPaths()
}

// SensitiveFragments 供原文兜底扫描的特征片段：自保护相对数据根片段 + 目录文件 fragments。
func SensitiveFragments() []string {
	return NewPathCatalog().fragments
}

// PathHitsSensitiveFragment 路径文本含目录文件声明的特征片段（边界匹配）。
// 供只有路径文本、没有真实家目录语境的调用方（用户输入 @file 引用展开）补一道判定：
// /root/.aws/credentials 这类不在当前用户家目录下的凭据路径，目录匹配不到，片段仍能识别。
func PathHitsSensitiveFragment(p string) bool {
	return hitsFragment(p, SensitiveFragments())
}
