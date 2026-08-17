package server

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"logviewer/internal/cmdbuild"
	"logviewer/internal/config"
	"logviewer/internal/metrics"
	"logviewer/internal/procmgr"
)

// countingWriter 包装一个 io.Writer，累计写入字节数。用于把导出量计入
// Prometheus 指标（logviewer_export_bytes_total）。
type countingWriter struct {
	w    io.Writer
	kind string
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	metrics.IncExportBytes(c.kind, int64(n))
	return n, err
}

// contentDisposition 构造 Content-Disposition 响应头，正确处理非 ASCII 文件名。
//
// 同时给 `filename="..."`（ASCII 回退，非 ASCII 用 '_' 替换以保证头字段合法）
// 和 RFC 5987 的 `filename*=UTF-8''<percent-encoded>`（现代浏览器优先采用）。
// 否则中文文件名在下载时会变成乱码或被截断。
func contentDisposition(filename string) string {
	ascii := make([]rune, 0, len(filename))
	for _, r := range filename {
		if r < 0x20 || r == '"' || r == '\\' || r > 0x7e {
			ascii = append(ascii, '_')
		} else {
			ascii = append(ascii, r)
		}
	}
	return fmt.Sprintf(`attachment; filename="%s"; filename*=UTF-8''%s`,
		string(ascii), url.PathEscape(filename))
}

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
//
// 关键：同时排空 stderr。导出是流式输出，一旦首字节写出就无法再改 HTTP 状态码；
// 但如果命令在写出任何字节前就失败（文件不存在、权限拒绝等），我们还没发响应头，
// 此时把 stderr 转成 JSON 错误返回 4xx/5xx，避免用户下到一个 0 字节的"成功"文件。
func streamProcessToResponse(c *gin.Context, p procmgr.Process, metricKind string) {
	stdout, err := p.StdoutPipe()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "管道错误: " + err.Error()})
		return
	}
	stderr, err := p.StderrPipe()
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
	var killOnce sync.Once
	killProc := func() { _ = p.Kill() }
	defer func() {
		killOnce.Do(killProc)
		_ = p.Wait()
	}()

	// 监听客户端断开，终止命令进程
	go func() {
		select {
		case <-ctx.Done():
			killOnce.Do(killProc)
		case <-done:
		}
	}()

	// 并发排空 stderr，命令失败时用于组装可读错误。
	// 只保留前 4KB：错误只取首行，无需累积全部 stderr（防止异常命令持续刷错撑爆内存）。
	var errBuf bytes.Buffer
	errDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(&limitWriter{w: &errBuf, n: 4096}, stderr)
		close(errDone)
	}()

	// 用 firstByteWriter 拦截首字节写出：在它之前若进程提前退出且无输出，
	// 响应头尚未发送，可以安全地改回 JSON 错误。
	fbw := &firstByteWriter{w: &countingWriter{w: c.Writer, kind: metricKind}}
	_, copyErr := io.Copy(fbw, stdout)
	<-errDone
	close(done)

	if !fbw.wrote {
		// 一个字节都没写出：进程可能因错误退出，把 stderr 作为错误返回。
		if msg := strings.TrimSpace(errBuf.String()); msg != "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "导出失败: " + firstLine(msg)})
			return
		}
		if copyErr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "导出失败: " + copyErr.Error()})
			return
		}
		// 正常空结果：什么都不写也合法（空文件），交由上层已设置的下载头。
	}
	// 已写出过字节：响应头已发，中途错误无法再改状态码（仅服务端可见）。
	if copyErr != nil {
		slog.Error("导出流式输出中途出错", "kind", metricKind, "err", copyErr)
	}
}

// firstByteWriter 在首次写入前记录 wrote=false，写入发生后 wrote=true。
type firstByteWriter struct {
	w     io.Writer
	wrote bool
}

func (f *firstByteWriter) Write(p []byte) (int, error) {
	if len(p) > 0 {
		f.wrote = true
	}
	return f.w.Write(p)
}

// limitWriter 最多接收前 n 字节，后续丢弃。
type limitWriter struct {
	w io.Writer
	n int64
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if l.n <= 0 {
		return len(p), nil
	}
	if int64(len(p)) > l.n {
		p = p[:l.n]
	}
	n, err := l.w.Write(p)
	l.n -= int64(n)
	return len(p), err
}

// firstLine 返回多行文本的第一行非空内容，用于把进程 stderr 压成单行错误。
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return s
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
	fi, statErr := h.Stat(abs)
	if statErr != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "文件不存在或无法访问: " + statErr.Error()})
		return
	}
	if fi.IsDir() {
		c.JSON(http.StatusBadRequest, gin.H{"error": "所选为目录，请选择日志文件"})
		return
	}
	c.Header("Content-Length", fmt.Sprintf("%d", fi.Size()))
	c.Header("Content-Disposition", contentDisposition(exportFileName(baseName(abs))))
	c.Header("Content-Type", "application/octet-stream")

	rc, err := h.Open(abs)
	if err != nil {
		// 响应头可能尚未 flush，尝试回 JSON；若已 flush 则只能记录日志。
		c.JSON(http.StatusBadRequest, gin.H{"error": "打开文件失败: " + err.Error()})
		return
	}
	defer rc.Close()
	_, _ = io.Copy(&countingWriter{w: c.Writer, kind: "origin"}, rc)
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
	// 在写下载头/拼命令前拦截越界参数，避免 200 流里启动病态命令失败或读爆内存。
	if err := cfg.Validate(); err != nil {
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
	// 正则非法时必须在写响应头【之前】拦截：导出是流式输出，一旦写了下载头，
	// 后续就无法再用 JSON 报错，进程的 stderr 也不会展示给用户。
	if msgErr := validateFilter(h, f); msgErr != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": msgErr})
		return
	}
	// 导出是静态读取（BuildExport 固定 static 模式），文件不存在必须直接 4xx，
	// 否则会写出 200 + 0 字节的"假成功"文件，用户无从知道原因。
	if fi, statErr := h.Stat(abs); statErr != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "文件不存在或无法访问: " + statErr.Error()})
		return
	} else if fi.IsDir() {
		c.JSON(http.StatusBadRequest, gin.H{"error": "所选为目录，请选择日志文件"})
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
	c.Header("Content-Disposition", contentDisposition(filteredName))
	c.Header("Content-Type", "text/plain; charset=utf-8")

	exportCmd := cmdbuild.BuildExport(h.Platform(), abs, cfg.Encoding, cfg.ReadLinesLimit, f)
	slog.Info("导出日志", "host", h.Name(), "file", baseName(abs), "shell", exportCmd.Shell)
	proc, err := h.Run(exportCmd)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "启动命令失败: " + err.Error()})
		return
	}
	streamProcessToResponse(c, proc, "filter")
}
