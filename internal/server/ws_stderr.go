package server

import "strings"

// stderrClass 表示一行子进程 stderr 的分类结果。
type stderrClass int

const (
	stderrBenign  stderrClass = iota // 良性提示，忽略
	stderrError                     // 真实错误，染红展示
	stderrRotate                    // 日志文件被轮转/替换（tail -F 继续跟踪新文件）
	stderrTruncate                  // 日志文件被截断（tail -F 继续跟踪新内容）
)

// classifyStderr 决定一条子进程 stderr 行是错误、轮转/截断通知还是良性噪声。
//
// 背景：`tail -F` 在正常工作时也会往 stderr 打印信息性消息（文件被新建、被截断、
// 被滚动替换等）。这些不是错误，旧实现把它们一律染红，用户会看到诸如
// "'xxx.log' 已被建立；正在跟随新文件的末尾" 的伪错误；或更糟，把轮转信息
// 整个丢弃，用户无从知道为何日志"跳变"。
//
// "文件已出现/建立"（has appeared）属于首次跟踪的正常事件，忽略即可；
// "被替换/轮转"（replaced）与"被截断"（truncated）是运行中值得告知用户的事件，
// 归类为对应 notice，由前端显示为可关闭的非红色提示条。
func classifyStderr(line string) stderrClass {
	s := strings.TrimSpace(trimLine(line))
	if s == "" {
		return stderrBenign
	}
	if !strings.HasPrefix(s, "tail:") {
		// 非 tail 前缀（grep/awk/iconv 等的报错）一律视为真实错误。
		return stderrError
	}
	normalized := strings.Join(strings.Fields(s), " ")

	rotateMarkers := []string{
		"has been replaced; following new file", // GNU/coreutils 英文
		"已被替换；正在跟随新文件的末尾",          // GNU/coreutils 中文
	}
	for _, m := range rotateMarkers {
		if strings.Contains(normalized, m) {
			return stderrRotate
		}
	}
	truncateMarkers := []string{
		"file truncated",       // GNU/coreutils 英文
		"文件已截断",            // GNU/coreutils 中文
	}
	for _, m := range truncateMarkers {
		if strings.Contains(normalized, m) {
			return stderrTruncate
		}
	}
	appearMarkers := []string{
		"has appeared; following new file", // GNU/coreutils 英文
		"已被建立；正在跟随新文件的末尾",      // GNU/coreutils 中文
	}
	for _, m := range appearMarkers {
		if strings.Contains(normalized, m) {
			return stderrBenign
		}
	}
	// 其它 tail: 前缀消息保守视为错误（便于暴露未识别的异常）。
	return stderrError
}
