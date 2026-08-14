// Package host 抽象日志来源机器：本机（LocalHost）或远程 SSH（阶段二）。
// server 层只依赖 Host 接口，不再直接调用 os.*/exec.* 或 runtime.GOOS，
// 从而让同一套 HTTP/WS 逻辑既能服务本机也能服务远程机器。
package host

import (
	"errors"
	"io"
	"os"

	"logviewer/internal/cmdbuild"
	"logviewer/internal/config"
	"logviewer/internal/procmgr"
)

// ErrHostNotFound 表示请求的机器别名不存在。
var ErrHostNotFound = errors.New("机器不存在或未配置")

// Node 目录树节点（与前端约定的 JSON 结构）。
type Node struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	IsDir   bool   `json:"isDir"`
	Size    int64  `json:"size"`
	ModTime string `json:"modTime"`
	HasLog  bool   `json:"hasLog"`
}

// Info 描述一台机器的概要信息，供顶栏切换器使用。
type Info struct {
	Name      string `json:"name"`
	Platform  string `json:"platform"`
	Local     bool   `json:"local"`
	Online    bool   `json:"online"`
	Available bool   `json:"available"`
	Message   string `json:"message,omitempty"`
}

// Host 是一台可浏览日志、可执行原生命令的机器。
// 所有路径参数都是目标机器上的路径；实现负责路径穿越校验与平台适配。
type Host interface {
	Name() string
	Platform() string
	Info() Info
	Dirs() []string
	Configs() *config.Manager
	// Capabilities 返回远端命令能力（本机恒为全部 true）。
	Capabilities() Capabilities

	// ResolvePath 校验 p 在允许的根目录内（含符号链接逃逸检测），
	// 返回可用于后续 Stat/Open/Run 的规范化路径。
	ResolvePath(p string) (string, error)

	Ls(dir string) ([]Node, error)
	Stat(path string) (os.FileInfo, error)
	Open(path string) (io.ReadCloser, error)

	// Run 在目标机器上执行命令管道，返回可被 procmgr 管控的进程。
	Run(cmd cmdbuild.Command) (procmgr.Process, error)
}

// Manager 按别名管理所有 Host。
type Manager struct {
	order []string
	hosts map[string]Host
}

// NewManager 构造 Manager。传入的 host 名称不能重复。
func NewManager(hosts ...Host) (*Manager, error) {
	m := &Manager{hosts: map[string]Host{}}
	for _, h := range hosts {
		name := h.Name()
		if name == "" {
			return nil, errors.New("host 名称不能为空")
		}
		if _, exists := m.hosts[name]; exists {
			return nil, errors.New("host 名称重复: " + name)
		}
		m.hosts[name] = h
		m.order = append(m.order, name)
	}
	return m, nil
}

// Get 按别名取 Host，不存在返回 ErrHostNotFound。
func (m *Manager) Get(name string) (Host, error) {
	if h, ok := m.hosts[name]; ok {
		return h, nil
	}
	return nil, ErrHostNotFound
}

// List 返回所有机器的概要信息（按注册顺序）。
func (m *Manager) List() []Info {
	out := make([]Info, 0, len(m.order))
	for _, name := range m.order {
		out = append(out, m.hosts[name].Info())
	}
	return out
}

// Names 返回所有机器别名（按注册顺序）。
func (m *Manager) Names() []string {
	return append([]string(nil), m.order...)
}

// Close 关闭所有实现了 io.Closer 的机器（目前是 SSHHost：断开 SSH/SFTP 与 keepalive）。
// 在服务优雅关闭时调用，确保连接与后台协程被回收。LocalHost 无需关闭。
func (m *Manager) Close() {
	for _, name := range m.order {
		if c, ok := m.hosts[name].(interface{ Close() error }); ok {
			_ = c.Close()
		}
	}
}
