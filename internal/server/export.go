package server

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"logviewer/internal/cmdbuild"
	"logviewer/internal/config"
	"logviewer/internal/procmgr"
)

// 导出文件名规则：原名_时间戳.后缀
func exportFileName(base string) string {
	ts := time.Now().Format("20060102_150405")
	ext := filepath.Ext(base)
	name := strings.TrimSuffix(base, ext)
	return fmt.Sprintf("%s_%s%s", name, ts, ext)
}

// baseName 取路径最后一段作为文件名，同时识别 / 和 \ 分隔符。
// 不能用 filepath.Base：远程路径的分隔符可能与服务器本机不同（如 Linux 服务器访问 Windows 远端）。
func baseName(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}

// streamProcessToResponse 启动进程并把 stdout 流式写到 HTTP 响应。
// 整个读取/过滤都由原生命令完成，Go 只做字节转发（纯外壳）。
// 客户端断开时通过 context 取消终止命令进程，避免孤儿进程。
func streamProcessToResponse(c *gin.Context, p procmgr.Process) {
	stdout, err := p.StdoutPipe()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "管道错误: " + err.Error()})
		return
	}
	if err := p.Start(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "命令启动失败: " + err.Error()})
		return
	}

	ctx := c.Request.Context()
	done := make(chan struct{})
	defer func() {
		_ = p.Kill()
		_ = p.Wait()
	}()

	// 监听客户端断开，终止命令进程
	go func() {
		select {
		case <-ctx.Done():
			_ = p.Kill()
		case <-done:
		}
	}()

	if _, err := io.Copy(c.Writer, stdout); err != nil {
		// 客户端中断等：不写 JSON（头可能已发），直接返回
		close(done)
		return
	}
	close(done)
}

// handleDownloadOrigin 下载原始日志文件（字节级原样输出）。
// 走 Host.Open（本机 os.Open / 远程 SFTP），不经过过滤命令。
// 已知文件大小时设置 Content-Length，前端可显示真实进度百分比。
func (s *Server) handleDownloadOrigin(c *gin.Context) {
	h, ok := s.hostFrom(c)
	if !ok {
		return
	}
	abs, err := h.ResolvePath(c.Query("path"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if fi, statErr := h.Stat(abs); statErr == nil {
		c.Header("Content-Length", fmt.Sprintf("%d", fi.Size()))
	}
	c.Header("Content-Disposition", `attachment; filename="`+exportFileName(baseName(abs))+`"`)
	c.Header("Content-Type", "application/octet-stream")

	rc, err := h.Open(abs)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "打开文件失败: " + err.Error()})
		return
	}
	defer rc.Close()
	_, _ = io.Copy(c.Writer, rc)
}

// handleDownloadFilter 导出过滤后的日志。
// 接收前端当前表单的完整配置（POST JSON），保证"导出过滤"用的就是用户眼前正在调的规则，
// 而不是某个已保存配置名。
func (s *Server) handleDownloadFilter(c *gin.Context) {
	h, ok := s.hostFrom(c)
	if !ok {
		return
	}
	abs, err := h.ResolvePath(c.Query("path"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	var cfg config.LogConfig
	if err := c.ShouldBindJSON(&cfg); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "参数错误: " + err.Error()})
		return
	}
	ts, te, _ := cmdbuild.TimeBounds(cfg.FilterRule)
	f := cmdbuild.FilterCfg{
		Pattern:       cmdbuild.AssemblePattern(cfg.FilterRule, cfg.UseRegex),
		Exclude:       cfg.FilterRule.Exclude,
		TimeStart:     ts,
		TimeEnd:       te,
		UseRegex:      cfg.UseRegex,
		CaseSensitive: cfg.CaseSensitive,
		InvertMatch:   cfg.InvertMatch,
		ContextBefore: cfg.ContextBefore,
		ContextAfter:  cfg.ContextAfter,
	}
	if cfg.UseRegex && cfg.FilterRule.CustomRegex != "" {
		f.TimeStart, f.TimeEnd = "", ""
	}
	if msgErr := checkCaps(h, cfg.Encoding, f.TimeStart != "" || f.TimeEnd != ""); msgErr != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": msgErr})
		return
	}
	base := exportFileName(baseName(abs))
	dot := strings.LastIndex(base, ".")
	var filteredName string
	if dot > 0 {
		filteredName = base[:dot] + "_filtered" + base[dot:]
	} else {
		filteredName = base + "_filtered"
	}
	c.Header("Content-Disposition", `attachment; filename="`+filteredName+`"`)
	c.Header("Content-Type", "text/plain; charset=utf-8")

	exportCmd := cmdbuild.BuildExport(h.Platform(), abs, cfg.Encoding, cfg.ReadLinesLimit, f)
	log.Printf("[export] host=%s file=%s shell=%s", h.Name(), baseName(abs), exportCmd.Shell)
	proc, err := h.Run(exportCmd)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "启动命令失败: " + err.Error()})
		return
	}
	streamProcessToResponse(c, proc)
}
