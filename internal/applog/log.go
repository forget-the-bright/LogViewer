// Package applog 统一 LogViewer 服务端自身的日志输出。
//
// 在进程启动早期（加载配置后、构造 server 前）调用 Init，按配置选择：
//   - JSON 或文本 handler（log_json=true 输出 JSON，便于日志采集系统消费）；
//   - 日志级别（debug/info/warn/error）。
//
// 同时把标准库 log 包（log.Printf 等既有调用点）的输出重定向到 slog，
// 保证未迁移的调用不丢、格式统一，迁移可渐进进行。
package applog

import (
	"io"
	"log"
	"log/slog"
	"os"
	"strings"
)

// Init 配置全局 slog logger，并重定向标准库 log 到它。
// json 为 true 用 JSON handler，否则用文本 handler；level 取 debug/info/warn/error。
// 重复调用会重置全局 logger。输出写到 os.Stderr。
func Init(json bool, level string) {
	InitWithOutput(json, level, os.Stderr)
}

// InitWithOutput 同 Init，但允许指定日志写入目标。GUI 模式（-H windowsgui）
// 没有控制台，传入日志文件避免 slog/gin 日志丢失。
// 重复调用会重置全局 logger。
func InitWithOutput(json bool, level string, w io.Writer) {
	lvl := parseLevel(level)
	opts := &slog.HandlerOptions{Level: lvl}
	var h slog.Handler
	if json {
		h = slog.NewJSONHandler(w, opts)
	} else {
		h = slog.NewTextHandler(w, opts)
	}
	logger := slog.New(h)
	slog.SetDefault(logger)

	// 把标准库 log.Printf/Fatalf 等重写到 slog（按文本消息记为 INFO，
	// 因为这些调用点没有级别语义；高价值点已直接改用 slog）。
	log.SetFlags(0)
	log.SetOutput(&stdlogBridge{l: logger})
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// stdlogBridge 让旧的 log.Printf 输出进入 slog。
type stdlogBridge struct{ l *slog.Logger }

func (b *stdlogBridge) Write(p []byte) (int, error) {
	b.l.Info(strings.TrimRight(string(p), " \r\n"))
	return len(p), nil
}
