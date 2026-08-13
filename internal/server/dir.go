package server

import (
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// Node 目录树节点
type Node struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	IsDir    bool   `json:"isDir"`
	Size     int64  `json:"size"`
	ModTime  string `json:"modTime"`
	HasLog   bool   `json:"hasLog"` // 递归含 log/out 文件（用于前端懒加载判断是否可展开）
}

// 允许展示的文件后缀
var allowedExts = map[string]bool{".log": true, ".out": true}

// handleListRoots 返回允许的根工作目录
func (s *Server) handleListRoots(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"roots": s.rootsSnapshot()})
}

// handleListDir 返回指定目录的单层子节点（懒加载目录树）
// 目录节点全部展示；文件节点仅展示 .log / .out 后缀
func (s *Server) handleListDir(c *gin.Context) {
	raw := c.Query("path")
	abs, err := s.resolveAndCheck(raw)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	info, err := os.Stat(abs)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "目录不存在: " + err.Error()})
		return
	}
	if !info.IsDir() {
		c.JSON(http.StatusBadRequest, gin.H{"error": "不是目录: " + abs})
		return
	}

	entries, err := os.ReadDir(abs)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "读取目录失败: " + err.Error()})
		return
	}

	var nodes []Node
	for _, e := range entries {
		name := e.Name()
		child := filepath.Join(abs, name)
		isDir := e.IsDir()
		if !isDir && !allowedExts[strings.ToLower(filepath.Ext(name))] {
			continue // 非目录且非 log/out 文件，隐藏
		}
		n := Node{Name: name, Path: child, IsDir: isDir}
		if f, err := e.Info(); err == nil {
			n.Size = f.Size()
			n.ModTime = f.ModTime().Format(time.RFC3339)
		}
		if isDir {
			n.HasLog = dirContainsLog(child)
		}
		nodes = append(nodes, n)
	}
	// 目录在前，文件在后，各自按名称排序
	sort.Slice(nodes, func(i, j int) bool {
		if nodes[i].IsDir != nodes[j].IsDir {
			return nodes[i].IsDir
		}
		return strings.ToLower(nodes[i].Name) < strings.ToLower(nodes[j].Name)
	})
	c.JSON(http.StatusOK, gin.H{"path": abs, "nodes": nodes})
}

// dirContainsLog 判断目录（单层）是否包含 log/out 文件或含日志的子目录（用于懒加载可展开标记）
func dirContainsLog(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() {
			return true
		}
		if allowedExts[strings.ToLower(filepath.Ext(e.Name()))] {
			return true
		}
	}
	return false
}