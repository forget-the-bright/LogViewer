package server

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// handleListRoots 返回指定机器允许的根工作目录。
func (s *Server) handleListRoots(c *gin.Context) {
	h, ok := s.hostFrom(c)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, gin.H{"roots": h.Dirs()})
}

// handleListDir 返回指定目录的单层子节点（懒加载目录树）。
// 目录节点全部展示；文件节点仅展示 .log / .out 后缀。
func (s *Server) handleListDir(c *gin.Context) {
	h, ok := s.hostFrom(c)
	if !ok {
		return
	}
	raw := c.Query("path")
	nodes, err := h.Ls(raw)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	// ResolvePath 已在 Ls 内部完成，这里回显规范化后的路径
	abs, err := h.ResolvePath(raw)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"path": abs, "nodes": nodes})
}
