package server

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"logviewer/internal/cmdbuild"
	"logviewer/internal/config"
)

// handleConfigPreview 返回时间范围与匹配正则的可读拼装（用于前端实时预览），
// 并在目标机器的原生引擎上做空跑语法校验，把正则错误实时反馈给用户。
func (s *Server) handleConfigPreview(c *gin.Context) {
	h, ok := s.hostFrom(c)
	if !ok {
		return
	}
	var req struct {
		FilterRule    config.FilterRule `json:"FilterRule"`
		UseRegex      bool              `json:"UseRegex"`
		CaseSensitive bool              `json:"CaseSensitive"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "参数错误: " + err.Error()})
		return
	}

	ts, te, timeErr := cmdbuild.TimeBounds(req.FilterRule)
	f := cmdbuild.FilterCfg{
		Pattern:       cmdbuild.AssemblePattern(req.FilterRule, req.UseRegex),
		Exclude:       req.FilterRule.Exclude,
		TimeStart:     ts,
		TimeEnd:       te,
		UseRegex:      req.UseRegex,
		CaseSensitive: req.CaseSensitive,
	}
	if req.UseRegex && req.FilterRule.CustomRegex != "" {
		f.TimeStart, f.TimeEnd = "", ""
	}

	// 实时校验（在用户停顿 150ms 后触发）：
	//   - 时间范围解析错误（自定义正则会覆盖时间范围，此时时间错误无意义，不提示）
	//   - 正则语法错误（仅正则模式需要：非正则模式下内容/级别均经 QuoteMeta 转义）
	// 非法时仍返回拼装结果，但带上 regexError，前端据此把输入框标红并展示错误。
	var regexError string
	if timeErr != nil && !(req.UseRegex && req.FilterRule.CustomRegex != "") {
		regexError = timeErr.Error()
	} else if req.UseRegex {
		if msg := s.validateFilter(h, f); msg != "" {
			regexError = msg
		}
	}

	var timeDesc string
	switch {
	case f.TimeStart != "" && f.TimeEnd != "":
		timeDesc = f.TimeStart + "  ~  " + f.TimeEnd
	case f.TimeStart != "":
		timeDesc = f.TimeStart + "  ~  (无终点)"
	case f.TimeEnd != "":
		timeDesc = "(无起点)  ~  " + f.TimeEnd
	}
	pattern := f.Pattern
	if pattern == "" {
		pattern = "(无内容正则)"
	}
	if timeDesc == "" {
		timeDesc = "(不限时间)"
	}
	c.JSON(http.StatusOK, gin.H{
		"pattern":    pattern,
		"timeRange":  timeDesc,
		"regexError": regexError,
	})
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
	// 保存前校验数值边界：否则越界的 ReadLinesLimit/ContextBefore 会被写入磁盘，
	// 直到查看/导出时才报错，用户保存时却看到成功，预设配置形同损坏。
	if err := cfg.Validate(); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
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
