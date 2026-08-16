package host

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/pkg/sftp"
)

// 本文件实现 SSHHost 的路径校验与文件操作（Ls/Stat/Open）。
// 路径处理不能用本机 filepath：服务器可能是 Linux 而远端是 Windows（盘符、反斜杠、
// 大小写不敏感），反之亦然。因此这里用按平台分隔符参数化的纯字符串算法做词法清洗，
// 再用 SFTP RealPath 解析符号链接做越界判定。

func remoteSep(platform string) string {
	if platform == "windows" {
		return "\\"
	}
	return "/"
}

func isDriveLetter(b byte) bool {
	return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z')
}

// remoteClean 用目标平台的分隔符做路径清洗（等价 filepath.Clean，但不依赖本机 OS）。
//   - windows：把 / 归一为 \，识别盘符 X: 与 UNC \\server\share
//   - unix：仅按 / 分隔
func remoteClean(p, sep string) string {
	windows := sep == "\\"
	if windows {
		p = strings.ReplaceAll(p, "/", "\\")
	}
	if p == "" {
		return "."
	}

	volLen := 0
	if windows && len(p) >= 2 && p[1] == ':' && isDriveLetter(p[0]) {
		volLen = 2
	}
	vol := p[:volLen]
	rest := p[volLen:]

	// UNC：\\server\share 把 \\server\share 作为不可回退的根前缀。
	uncPrefix := ""
	if windows && volLen == 0 && strings.HasPrefix(p, "\\\\") {
		trimmed := strings.TrimPrefix(p, "\\\\")
		parts := strings.Split(trimmed, sep)
		if len(parts) >= 2 {
			uncPrefix = "\\\\" + parts[0] + sep + parts[1]
			if len(parts) > 2 {
				rest = sep + strings.Join(parts[2:], sep)
			} else {
				rest = sep
			}
		}
	}

	rooted := strings.HasPrefix(rest, sep)
	parts := strings.Split(rest, sep)

	var stack []string
	for _, comp := range parts {
		if comp == "" || comp == "." {
			continue
		}
		if comp == ".." {
			if len(stack) > 0 && stack[len(stack)-1] != ".." {
				stack = stack[:len(stack)-1]
			} else if !rooted {
				stack = append(stack, "..")
			}
			continue
		}
		stack = append(stack, comp)
	}

	out := vol + uncPrefix
	if rooted {
		out += sep
	}
	out += strings.Join(stack, sep)
	if out == "" {
		out = vol
	}
	if out == "" {
		out = "."
	}
	// Windows 盘根归一为 X:\
	if windows && volLen == 2 && len(out) == 2 {
		out += sep
	}
	return out
}

// remoteIsAbs 判断是否为目标平台的绝对路径。
func remoteIsAbs(p, sep string) bool {
	if sep == "\\" {
		// 盘符绝对路径：X:\ 或 X:/...（X: 无分隔符是盘相对路径，不算绝对）
		if len(p) >= 3 && p[1] == ':' && isDriveLetter(p[0]) && (p[2] == '\\' || p[2] == '/') {
			return true
		}
		if strings.HasPrefix(p, "\\\\") || strings.HasPrefix(p, "//") {
			return true
		}
		return false
	}
	return strings.HasPrefix(p, "/")
}

// remoteWithin 判断 p 是否等于 root 或位于 root 之下（两者都需先经 remoteClean）。
// windows 比较时大小写不敏感。不能用字符串前缀直接判断，必须加分隔符边界。
func remoteWithin(p, root, sep string, caseFold bool) bool {
	a, b := p, root
	if caseFold {
		a = strings.ToLower(a)
		b = strings.ToLower(b)
	}
	if a == b {
		return true
	}
	return strings.HasPrefix(a, b+sep)
}

func remoteParent(p, sep string) (parent, base string) {
	i := strings.LastIndex(p, sep)
	if i < 0 {
		return "", p
	}
	// Windows 盘根 X:\file
	if sep == "\\" && i == 2 && len(p) > 2 && p[1] == ':' {
		return p[:3], p[3:]
	}
	parent = p[:i]
	base = p[i+1:]
	if parent == "" && sep == "/" {
		parent = "/"
	}
	return parent, base
}

func remoteJoin(parent, base, sep string) string {
	if strings.HasSuffix(parent, sep) {
		return parent + base
	}
	return parent + sep + base
}

// realRootsCached 返回已解析的根目录真实路径（连接后预计算）；未连接时回退词法清洗。
func (h *SSHHost) realRootsCached(sep string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.realRoots) > 0 {
		return append([]string(nil), h.realRoots...)
	}
	roots := make([]string, len(h.dirs))
	for i, d := range h.dirs {
		roots[i] = remoteClean(d, sep)
	}
	return roots
}

// ResolvePath 校验 p 在允许根目录内，并做符号链接逃逸检测。
func (h *SSHHost) ResolvePath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", &PathError{msg: "路径为空"}
	}
	if err := h.ensureConnected(); err != nil {
		return "", err
	}
	platform := h.Platform()
	sep := remoteSep(platform)
	windows := platform == "windows"

	abs := remoteClean(p, sep)
	if !remoteIsAbs(abs, sep) {
		return "", &PathError{msg: "远程路径必须是绝对路径: " + p}
	}

	cleanRoots := make([]string, len(h.dirs))
	for i, r := range h.dirs {
		cleanRoots[i] = remoteClean(r, sep)
	}
	matched := false
	for _, r := range cleanRoots {
		if remoteWithin(abs, r, sep, windows) {
			matched = true
			break
		}
	}
	if !matched {
		return "", &PathError{msg: "访问超出允许范围: " + p}
	}

	// SFTP RealPath 解析符号链接；文件可能暂不存在（跟踪模式），退化到父目录。
	var real string
	err := h.withSFTP(func(sc *sftp.Client) error {
		rp, e := sc.RealPath(abs)
		if e != nil {
			parent, base := remoteParent(abs, sep)
			if parent != "" {
				if rp2, e2 := sc.RealPath(parent); e2 == nil {
					real = remoteJoin(remoteClean(rp2, sep), base, sep)
					return nil
				}
			}
			real = abs
			return nil
		}
		real = remoteClean(rp, sep)
		return nil
	})
	if err != nil {
		return "", err
	}

	for _, rr := range h.realRootsCached(sep) {
		if remoteWithin(real, rr, sep, windows) {
			return abs, nil
		}
	}
	return "", &PathError{msg: "访问超出允许范围（符号链接逃逸）: " + p}
}

// Ls 列出单层子节点。目录全展示，文件按配置的后缀过滤（默认 .log/.out）。
func (h *SSHHost) Ls(dir string) ([]Node, error) {
	abs, err := h.ResolvePath(dir)
	if err != nil {
		return nil, err
	}
	var infos []os.FileInfo
	err = h.withSFTP(func(sc *sftp.Client) error {
		var e error
		infos, e = sc.ReadDir(abs)
		return e
	})
	if err != nil {
		return nil, fmt.Errorf("读取远程目录失败: %w", err)
	}

	sep := remoteSep(h.Platform())
	var nodes []Node
	for _, info := range infos {
		name := info.Name()
		isDir := info.IsDir()
		if !isDir && !extAllow(name, h.exts, h.showAll) {
			continue
		}
		child := remoteJoin(abs, name, sep)
		n := Node{
			Name:    name,
			Path:    child,
			IsDir:   isDir,
			Size:    info.Size(),
			ModTime: info.ModTime().Format(time.RFC3339),
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

// Stat 返回远程文件信息。path 必须是 ResolvePath 已返回的规范化绝对路径；
// 这里不再重复 ResolvePath（SSH 下会多一次 SFTP RealPath 往返）。
func (h *SSHHost) Stat(path string) (os.FileInfo, error) {
	var info os.FileInfo
	err := h.withSFTP(func(sc *sftp.Client) error {
		var e error
		info, e = sc.Stat(path)
		return e
	})
	return info, err
}

// Open 打开远程文件用于读取（原始下载）。调用方负责 Close。
// path 必须是 ResolvePath 已返回的规范化绝对路径。
func (h *SSHHost) Open(path string) (io.ReadCloser, error) {
	var f *sftp.File
	err := h.withSFTP(func(sc *sftp.Client) error {
		var e error
		f, e = sc.Open(path)
		return e
	})
	if err != nil {
		return nil, fmt.Errorf("打开远程文件失败: %w", err)
	}
	return f, nil
}

// extName 取文件后缀（不依赖本机 filepath，远端可能是 Windows）。
func extName(name string) string {
	i := strings.LastIndexAny(name, "/\\")
	if i >= 0 {
		name = name[i+1:]
	}
	dot := strings.LastIndex(name, ".")
	if dot < 0 {
		return ""
	}
	return name[dot:]
}
