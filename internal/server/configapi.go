package server

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"logviewer/internal/cmdbuild"
	"logviewer/internal/config"
)

// handleConfigPreview 返回时间范围与匹配正则的可读拼装（用于前端实时预览）。
func (s *Server) handleConfigPreview(c *gin.Context) {
	var req struct {
		FilterRule config.FilterRule `json:"FilterRule"`
		UseRegex   bool              `json:"UseRegex"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "参数错误: " + err.Error()})
		return
	}
	var timeDesc, pattern string
	if req.UseRegex && req.FilterRule.CustomRegex != "" {
		pattern = req.FilterRule.CustomRegex
	} else {
		ts, te, ok := cmdbuild.TimeBounds(req.FilterRule)
		if ok {
			timeDesc = ts + "  ~  " + te
		}
		pattern = cmdbuild.AssemblePattern(req.FilterRule, req.UseRegex)
	}
	if pattern == "" {
		pattern = "(无内容正则)"
	}
	if timeDesc == "" && req.FilterRule.CustomRegex == "" {
		timeDesc = "(不限时间)"
	}
	c.JSON(http.StatusOK, gin.H{"pattern": pattern, "timeRange": timeDesc})
}

func (s *Server) handleConfigList(c *gin.Context) {
	h, ok := s.hostFrom(c)
	if !ok {
		return
	}
	mgr := h.Configs()
	c.JSON(http.StatusOK, gin.H{"names": mgr.List(), "default": mgr.GetDefault().ConfigName})
}

func (s *Server) handleConfigGet(c *gin.Context) {
	h, ok := s.hostFrom(c)
	if !ok {
		return
	}
	mgr := h.Configs()
	name := c.Query("name")
	if name == "" {
		c.JSON(http.StatusOK, mgr.GetDefault())
		return
	}
	if cfg, ok := mgr.Get(name); ok {
		c.JSON(http.StatusOK, cfg)
		return
	}
	c.JSON(http.StatusNotFound, gin.H{"error": "配置不存在: " + name})
}

func (s *Server) handleConfigSave(c *gin.Context) {
	h, ok := s.hostFrom(c)
	if !ok {
		return
	}
	var cfg config.LogConfig
	if err := c.ShouldBindJSON(&cfg); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "参数错误: " + err.Error()})
		return
	}
	if err := h.Configs().Save(cfg); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	mgr := h.Configs()
	c.JSON(http.StatusOK, gin.H{"ok": true, "names": mgr.List(), "default": mgr.GetDefault().ConfigName})
}

func (s *Server) handleConfigDelete(c *gin.Context) {
	h, ok := s.hostFrom(c)
	if !ok {
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "缺少配置名"})
		return
	}
	if err := h.Configs().Delete(req.Name); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	mgr := h.Configs()
	c.JSON(http.StatusOK, gin.H{"ok": true, "names": mgr.List(), "default": mgr.GetDefault().ConfigName})
}

func (s *Server) handleConfigRename(c *gin.Context) {
	h, ok := s.hostFrom(c)
	if !ok {
		return
	}
	var req struct {
		Old string `json:"old"`
		New string `json:"new"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Old == "" || req.New == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "缺少配置名"})
		return
	}
	if err := h.Configs().Rename(req.Old, req.New); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	mgr := h.Configs()
	c.JSON(http.StatusOK, gin.H{"ok": true, "names": mgr.List(), "default": mgr.GetDefault().ConfigName})
}

func (s *Server) handleConfigSetDefault(c *gin.Context) {
	h, ok := s.hostFrom(c)
	if !ok {
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "缺少配置名"})
		return
	}
	if err := h.Configs().SetDefault(req.Name); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "default": req.Name})
}
