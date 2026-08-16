package server

import "strings"

// classifyStderr 决定一条子进程 stderr 行是否要作为错误显示给用户。
//
// 背景：`tail -F` 在正常工作时也会往 stderr 打印信息性消息（文件被新建、被截断、
// 被滚动替换等）。这些不是错误，旧实现把它们一律染红，用户会看到诸如
// "'xxx.log' 已被建立；正在跟随新文件的末尾" 的伪错误。
//
// 返回空串表示该行是良性提示、应忽略；否则返回应展示的文本（已去除首尾空白）。
func classifyStderr(line string) string {
	s := strings.TrimSpace(trimLine(line))
	if s == "" {
		return ""
	}
	if isBenignTail(s) {
		return ""
	}
	return s
}

// isBenignTail 判断一行 tail -F 的输出是否为良性提示（而非真实错误）。
// 用 strings.Fields 归一化空白，规避 GNU tail 文案中 "has appeared;  following"
// 这类多空格差异。
func isBenignTail(s string) bool {
	if !strings.HasPrefix(s, "tail:") {
		return false
	}
	normalized := strings.Join(strings.Fields(s), " ")
	benignMarkers := []string{
		// GNU/coreutils 英文：
		"has appeared; following new file",
		"has been replaced; following new file",
		"file truncated",
		// GNU/coreutils 中文（按实测 locale 文案）：
		"已被建立；正在跟随新文件的末尾",
		"已被替换；正在跟随新文件的末尾",
		"文件已截断",
	}
	for _, m := range benignMarkers {
		if strings.Contains(normalized, m) {
			return true
		}
	}
	return false
}
