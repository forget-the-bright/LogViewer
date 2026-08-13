package server

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"logviewer/internal/cmdbuild"
	"logviewer/internal/config"
)

// 导出文件名规则：原名_时间戳.后缀
func exportFileName(base string) string {
	ts := time.Now().Format("20060102_150405")
	ext := filepath.Ext(base)
	name := strings.TrimPrefix(base, filepath.Dir(base)+string(filepath.Separator))
	name = strings.TrimSuffix(name, ext)
	return fmt.Sprintf("%s_%s%s", name, ts, ext)
}

// streamCommandToResponse 运行一条命令并把 stdout 流式写到 HTTP 响应。
// 整个读取/过滤都由系统原生命令完成，Go 只做字节转发（纯外壳）。
// 用大块 io.Copy 转发，不在每个小块上 Flush，避免高频 flush 拖慢大文件导出。
func streamCommandToResponse(c *gin.Context, cmd *exec.Cmd) {
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "管道错误: " + err.Error()})
		return
	}
	if err := cmd.Start(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "命令启动失败: " + err.Error()})
		return
	}
	defer func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	}()

	if _, err := io.Copy(c.Writer, stdout); err != nil {
		// 客户端中断等：不写 JSON（头可能已发），直接返回
		return
	}
}

// handleDownloadOrigin 下载原始日志文件（用系统原生命令 cat / FileStream 原样输出）。
// 已知文件大小时设置 Content-Length，前端可显示真实进度百分比。
func (s *Server) handleDownloadOrigin(c *gin.Context) {
	abs, err := s.resolveAndCheck(c.Query("path"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if fi, statErr := os.Stat(abs); statErr == nil {
		c.Header("Content-Length", fmt.Sprintf("%d", fi.Size()))
	}
	c.Header("Content-Disposition", `attachment; filename="`+exportFileName(filepath.Base(abs))+`"`)
	c.Header("Content-Type", "application/octet-stream")
	streamCommandToResponse(c, cmdbuild.BuildOrigin(abs).BuildCmd())
}

// handleDownloadFilter 导出过滤后的日志。
// 接收前端当前表单的完整配置（POST JSON），保证"导出过滤"用的就是用户眼前正在调的规则，
// 而不是某个已保存配置名，避免与"导出原始"看起来无差别。
func (s *Server) handleDownloadFilter(c *gin.Context) {
	abs, err := s.resolveAndCheck(c.Query("path"))
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
	// 过滤导出文件名加 _filtered 后缀，与原始导出一眼区分
	base := exportFileName(filepath.Base(abs))
	dot := strings.LastIndex(base, ".")
	var filteredName string
	if dot > 0 {
		filteredName = base[:dot] + "_filtered" + base[dot:]
	} else {
		filteredName = base + "_filtered"
	}
	c.Header("Content-Disposition", `attachment; filename="`+filteredName+`"`)
	c.Header("Content-Type", "text/plain; charset=utf-8")
	exportCmd := cmdbuild.BuildExport(abs, cfg.Encoding, cfg.ReadLinesLimit, f)
	log.Printf("[export] 过滤导出命令 file=%s shell=%s\n%s", abs, exportCmd.Shell, exportCmd.Script)
	cmd := exportCmd.BuildCmd()
	streamCommandToResponse(c, cmd)
}