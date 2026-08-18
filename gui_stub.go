//go:build !(gui && windows)

package main

import (
	"errors"
	"io"
)

// supportsGUI 在非 GUI 构建（默认 web-only，或非 Windows 平台）恒为 false。
func supportsGUI() bool { return false }

// runGUI 在 web-only 构建中不可用；main 仅在 mode=gui/auto 需要 GUI 时才会调用。
func runGUI(svc *service) error {
	return errors.New("当前构建为 web-only（不含 GUI）；GUI 需在 Windows 上用 -tags gui 编译")
}

// guiLogOutput 在非 GUI 构建返回 nil，日志走 os.Stderr。
func guiLogOutput() io.Writer { return nil }
