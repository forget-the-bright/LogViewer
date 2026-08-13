package server

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"logviewer/internal/cmdbuild"
	"logviewer/internal/config"
)

// handleConfigPreview 返回时间范围与匹配正则的可读拼装（用于前端实时预览）。
// 时间范围由命令按字符串比较处理，不计入正则，保证正则短而清晰。
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
	// 自定义正则仅在"正则"勾选时生效；普通文本模式走拼装（内容按字面量）
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

// handleConfigList 返回所有配置名
func (s *Server) handleConfigList(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"names": s.cfg.List(), "default": s.cfg.GetDefault().ConfigName})
}

// handleConfigGet 返回指定配置
func (s *Server) handleConfigGet(c *gin.Context) {
	name := c.Query("name")
	if name == "" {
		c.JSON(http.StatusOK, s.cfg.GetDefault())
		return
	}
	if cfg, ok := s.cfg.Get(name); ok {
		c.JSON(http.StatusOK, cfg)
		return
	}
	c.JSON(http.StatusNotFound, gin.H{"error": "配置不存在: " + name})
}

// handleConfigSave 保存（新增或覆盖）
func (s *Server) handleConfigSave(c *gin.Context) {
	var cfg config.LogConfig
	if err := c.ShouldBindJSON(&cfg); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "参数错误: " + err.Error()})
		return
	}
	if err := s.cfg.Save(cfg); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "names": s.cfg.List(), "default": s.cfg.GetDefault().ConfigName})
}

// handleConfigDelete 删除配置
func (s *Server) handleConfigDelete(c *gin.Context) {
	var req struct {
		Name string `json:"name"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "缺少配置名"})
		return
	}
	if err := s.cfg.Delete(req.Name); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "names": s.cfg.List(), "default": s.cfg.GetDefault().ConfigName})
}

// handleConfigRename 重命名
func (s *Server) handleConfigRename(c *gin.Context) {
	var req struct {
		Old string `json:"old"`
		New string `json:"new"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Old == "" || req.New == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "缺少配置名"})
		return
	}
	if err := s.cfg.Rename(req.Old, req.New); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "names": s.cfg.List(), "default": s.cfg.GetDefault().ConfigName})
}

// handleConfigSetDefault 设为默认
func (s *Server) handleConfigSetDefault(c *gin.Context) {
	var req struct {
		Name string `json:"name"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "缺少配置名"})
		return
	}
	if err := s.cfg.SetDefault(req.Name); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "default": req.Name})
}