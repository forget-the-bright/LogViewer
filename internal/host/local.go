package host

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"logviewer/internal/cmdbuild"
	"logviewer/internal/config"
	"logviewer/internal/procmgr"
)

// LocalHost 是本机实现：直接用 os.* 访问文件、用 exec 启动原生命令。
type LocalHost struct {
	name      string
	platform  string
	roots     []string // 已 Abs+Clean 的根目录
	realRoots []string // roots 经 EvalSymlinks 后的真实路径（用于软链穿越校验）
	exts      map[string]bool
	showAll   bool // true 时展示所有文件（file_extensions 含 "*"）
	cfgMgr    *config.Manager
}

// NewLocalHost 构造本机 Host。
//   - dirs: 允许访问的根目录（会做 Abs+Clean+去重）；
//   - exts: 目录树中展示的文件后缀（nil 表示默认 .log/.out；含 "*" 展示全部）；
//   - initial: 初始过滤预设仓库；
//   - saveCfg: 预设变更时的持久化回调（可为 nil，表示不持久化）。
func NewLocalHost(name string, dirs []string, exts []string, initial config.ConfigStore, saveCfg config.SaveFunc) (*LocalHost, error) {
	if name == "" {
		name = "local"
	}
	extSet, showAll := normalizeExts(exts)
	h := &LocalHost{
		name:     name,
		platform: runtime.GOOS,
		exts:     extSet,
		showAll:  showAll,
		cfgMgr:   config.NewManager(initial, saveCfg),
	}
	for _, d := range dirs {
		h.addRoot(d)
	}
	if len(h.roots) == 0 {
		// 兜底：没有任何配置目录时用当前工作目录
		if cwd, err := os.Getwd(); err == nil {
			h.addRoot(cwd)
		}
	}
	return h, nil
}

func (h *LocalHost) addRoot(p string) {
	abs, err := filepath.Abs(strings.TrimSpace(p))
	if err != nil || abs == "" {
		return
	}
	abs = filepath.Clean(abs)
	for _, r := range h.roots {
		if r == abs {
			return
		}
	}
	h.roots = append(h.roots, abs)
	// 解析真实路径（不存在则原样保留）；用于软链逃逸检测
	real := abs
	if ev, err := filepath.EvalSymlinks(abs); err == nil {
		real = filepath.Clean(ev)
	}
	h.realRoots = append(h.realRoots, real)
}

func (h *LocalHost) Name() string             { return h.name }
func (h *LocalHost) Platform() string         { return h.platform }
func (h *LocalHost) Configs() *config.Manager { return h.cfgMgr }

// Fingerprint 返回本机实例的连接配置指纹，供 Manager.Rebuild 判断是否需要替换。
// 本机的可变身份是根目录集合（dirs）：热加载改了 dirs 就必须让新实例生效，
// 否则目录树永远停在旧集合上。name 已由 Manager 按别名匹配，这里只需编码 dirs。
func (h *LocalHost) Fingerprint() string {
	// 后缀集合计入指纹：改了 file_extensions 也要热加载生效（替换实例）。
	var extList []string
	if h.showAll {
		extList = []string{"*"}
	} else {
		for e := range h.exts {
			extList = append(extList, e)
		}
		sort.Strings(extList)
	}
	return "local|dirs=" + strings.Join(h.Dirs(), "|") + "|exts=" + strings.Join(extList, ",")
}

func (h *LocalHost) Info() Info {
	return Info{
		Name:      h.name,
		Platform:  h.platform,
		Local:     true,
		Online:    true,
		Available: true,
	}
}

func (h *LocalHost) Dirs() []string {
	out := make([]string, len(h.roots))
	copy(out, h.roots)
	return out
}

// Capabilities 本机假定所有命令可用（Windows 用 PowerShell 原生命令，无需 tail/grep/awk/iconv）。
func (h *LocalHost) Capabilities() Capabilities {
	return Capabilities{HasTail: true, HasCat: true, HasGrep: true, HasAwk: true, HasIconv: true}
}

// HealthCheck 本机恒可用。
func (h *LocalHost) HealthCheck() error { return nil }

// ResolvePath 校验 p 位于某个允许根目录内。
// 与旧实现相比新增了符号链接逃逸检测：若 p（或其父目录，当 p 不存在时）
// 经 EvalSymlinks 解析后跳出根目录，一律拒绝。
func (h *LocalHost) ResolvePath(p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		return "", &PathError{msg: "路径为空"}
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", &PathError{msg: "路径无效: " + err.Error()}
	}
	abs = filepath.Clean(abs)

	// 解析真实路径用于越界判断。文件不存在时（跟踪模式允许文件暂不存在），
	// 退化到对其父目录做解析。
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		if os.IsNotExist(err) {
			if ev, err2 := filepath.EvalSymlinks(filepath.Dir(abs)); err2 == nil {
				// 父目录真实路径 + 文件名基名
				real = filepath.Join(ev, filepath.Base(abs))
			} else {
				real = abs
			}
		} else {
			return "", &PathError{msg: "路径无效: " + err.Error()}
		}
	}
	real = filepath.Clean(real)

	for i, root := range h.roots {
		realRoot := h.realRoots[i]
		if within(abs, root) && within(real, realRoot) {
			// 返回 abs（保留用户可见路径，如含软链），实际文件操作由 OS 解析
			return abs, nil
		}
	}
	return "", &PathError{msg: "访问超出允许范围: " + p}
}

// within 判断 p 是否等于 root 或位于 root 之下。
// 不能用字符串前缀判断（/root 能被 /root-evil 绕过）。
func within(p, root string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	// rel == "." 即 p == root；其余合法情况是不以 ".." 开头（"../x" 或 ".." 本身都越界）。
	if rel == "." {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Ls 返回单层子节点（懒加载）。目录全展示，文件只展示 .log/.out。
func (h *LocalHost) Ls(dir string) ([]Node, error) {
	abs, err := h.ResolvePath(dir)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("目录不存在: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("不是目录: %s", abs)
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return nil, fmt.Errorf("读取目录失败: %w", err)
	}
	var nodes []Node
	for _, e := range entries {
		name := e.Name()
		child := filepath.Join(abs, name)
		isDir := e.IsDir()
		if !isDir && !extAllow(name, h.exts, h.showAll) {
			continue
		}
		n := Node{Name: name, Path: child, IsDir: isDir}
		if f, err := e.Info(); err == nil {
			n.Size = f.Size()
			n.ModTime = f.ModTime().Format(time.RFC3339)
		}
		nodes = append(nodes, n)
	}
	sort.Slice(nodes, func(i, j int) bool {
		if nodes[i].IsDir != nodes[j].IsDir {
			return nodes[i].IsDir
		}
		return strings.ToLower(nodes[i].Name) < strings.ToLower(nodes[j].Name)
	})
	return nodes, nil
}

// Stat 返回文件信息。path 必须是 ResolvePath 已返回的规范化绝对路径。
func (h *LocalHost) Stat(path string) (os.FileInfo, error) {
	return os.Stat(path)
}

// Open 打开文件用于读取（原始下载）。调用方负责 Close。
// path 必须是 ResolvePath 已返回的规范化绝对路径。
func (h *LocalHost) Open(path string) (io.ReadCloser, error) {
	return os.Open(path)
}

// Run 在本机启动命令。
func (h *LocalHost) Run(cmd cmdbuild.Command) (procmgr.Process, error) {
	return procmgr.LocalProcess(cmd.BuildCmd()), nil
}

// RunOneShot 在本机执行一条短命命令至结束，返回合并输出与退出码。
func (h *LocalHost) RunOneShot(cmd cmdbuild.Command) (string, int, error) {
	c := cmd.BuildCmd()
	out, err := c.CombinedOutput()
	if err == nil {
		return string(out), 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		// 命令自身非零退出：把退出码交给调用方判断（正则非法=2 等），不算基础设施错误。
		return string(out), exitErr.ExitCode(), nil
	}
	return string(out), -1, err
}

// PathError 路径安全相关错误。
type PathError struct{ msg string }

func (e *PathError) Error() string { return e.msg }
