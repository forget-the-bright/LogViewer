package server

import (
	"fmt"
	"strings"

	"logviewer/internal/cmdbuild"
	"logviewer/internal/host"
)

// validateFilter 在【真正执行过滤的那台机器的原生引擎】上空跑正则语法检查。
//
// 为什么不用 Go 的 regexp 编译：
//   - Unix 端用的是 grep -E（POSIX ERE），它【不】支持 (?:...) 非捕获组、\d/\s 等
//     PCRE 写法，而 Go RE2 支持；用 Go 校验会把这些非法串误判为合法，运行时仍报错。
//   - Windows 端用 .NET 正则，支持反向引用/回溯，又与 RE2 不同。
// 因此唯一可靠的校验是把模式交给目标引擎编译一次。这里不读取任何文件，开销仅为
// 一次进程/会话创建。
//
// 返回空串表示通过；否则返回面向用户的中文错误说明。
func validateFilter(h host.Host, f cmdbuild.FilterCfg) string {
	// 主匹配模式：grep -E / Select-String 始终按正则解释。
	// （非正则模式下内容经 QuoteMeta 转义，必然合法，但校验一次成本可忽略，保持统一。）
	if f.Pattern != "" {
		if msg := runRegexCheck(h, f.Pattern, f.CaseSensitive, "匹配正则"); msg != "" {
			return msg
		}
	}
	// 排除模式：仅"正则"模式下才是正则；非正则走 -F / SimpleMatch（字面量），无需校验。
	if f.UseRegex && strings.TrimSpace(f.Exclude) != "" {
		if msg := runRegexCheck(h, f.Exclude, f.CaseSensitive, "排除正则"); msg != "" {
			return msg
		}
	}
	return ""
}

// runRegexCheck 在目标机器上执行一次正则空跑校验，返回可读错误（空=通过）。
func runRegexCheck(h host.Host, pattern string, caseSensitive bool, label string) string {
	cmd := cmdbuild.BuildRegexCheck(h.Platform(), pattern, caseSensitive)
	out, exitCode, err := h.RunOneShot(cmd)
	if err != nil {
		// 连接/会话级失败：不要因此阻塞用户查看（命令稍后真正执行时仍会暴露同类错误），
		// 但给出明确提示，避免静默。
		return fmt.Sprintf("无法在目标机器校验%s（%v），请检查主机连接", label, err)
	}
	if exitCode == 0 {
		return ""
	}
	// 引擎报了语法错：取 stderr/合并输出的有效首行作为可读信息。
	detail := extractEngineError(out)
	if detail == "" {
		detail = fmt.Sprintf("退出码 %d", exitCode)
	}
	// 统一带上 label（"匹配正则"/"排除正则"）前缀：前端据此把出错的输入框
	// （包含框/排除框）标红。Windows 脚本自带的"正则语法错误"前缀先剥掉，
	// 避免出现"匹配正则正则语法错误"的重复措辞。
	detail = strings.TrimPrefix(detail, "正则语法错误")
	detail = strings.TrimPrefix(detail, "：")
	detail = strings.TrimPrefix(detail, ":")
	return fmt.Sprintf("%s语法错误：%s", label, detail)
}

// extractEngineError 从引擎输出中提取第一行非空文本，去掉命令名前缀噪声。
func extractEngineError(out string) string {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// 去掉形如 "grep: " 的命令前缀，保留核心信息。
		for _, prefix := range []string{"grep:", "grep "} {
			if strings.HasPrefix(line, prefix) {
				line = strings.TrimSpace(strings.TrimPrefix(line, prefix))
			}
		}
		return line
	}
	return ""
}
