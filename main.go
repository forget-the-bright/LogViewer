package main

import (
	"embed"
	"flag"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"

	"logviewer/internal/server"
)

//go:embed all:static
var staticFS embed.FS

func main() {
	addr := flag.String("addr", ":8080", "HTTP 监听地址")
	dir := flag.String("dir", "", "允许扫描的根工作目录（可多次使用的逗号分隔列表）")
	flag.Parse()

	// 程序目录（用于放置 config / exports）
	exe, err := os.Executable()
	baseDir := "."
	if err == nil {
		baseDir = filepath.Dir(exe)
	}
	configDir := filepath.Join(baseDir, "config")

	// 静态资源（允许 -static-dir 覆盖为外部目录，也便于开发调试）
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		log.Fatalf("加载静态资源失败: %v", err)
	}

	var roots []string
	if *dir != "" {
		for _, r := range splitList(*dir) {
			roots = append(roots, r)
		}
	}

	srv, err := server.New(configDir, sub, roots...)
	if err != nil {
		log.Fatalf("初始化服务失败: %v", err)
	}

	r := srv.Router()
	displayAddr := *addr
	if strings.HasPrefix(displayAddr, ":") {
		displayAddr = "127.0.0.1" + displayAddr
	}
	log.Printf("LogViewer 启动，访问 http://%s", displayAddr)
	if *addr == ":8080" {
		log.Printf("提示：静态资源已嵌入二进制，浏览器打开 http://%s 即可使用", displayAddr)
	}
	if err := r.Run(*addr); err != nil {
		log.Fatalf("服务启动失败: %v", err)
	}
}

func splitList(s string) []string {
	var out []string
	for _, p := range splitByComma(s) {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func splitByComma(s string) []string {
	var out []string
	var cur []rune
	for _, r := range s {
		if r == ',' || r == ';' {
			if len(cur) > 0 {
				out = append(out, string(cur))
				cur = nil
			}
			continue
		}
		cur = append(cur, r)
	}
	if len(cur) > 0 {
		out = append(out, string(cur))
	}
	return out
}