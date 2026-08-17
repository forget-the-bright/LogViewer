package cmdbuild

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"logviewer/internal/config"
)

// ===== 过滤规则处理 =====
//
// 设计原则（彻底修正）：
//   - 时间范围【不】用正则枚举每个时刻（范围一大正则就爆炸）。ISO 时间戳
//     "YYYY-MM-DD HH:MM:SS" 是定长字符串，字典序==时间序，因此交给命令做
//     字符串比较即可：Unix 用 awk，Windows 用 PowerShell，命令长度恒定。
//   - 日志级别 + 内容 才拼装成一条短正则（级别 OR，内容 AND，用 .* 连接）。
//   - 自定义正则优先级最高，覆盖一切。
//   - Go 只负责把界面配置翻译成命令参数，不处理日志数据。

// 时间戳在日志行中的形态（定长 19 字符，秒级）
const timeTokenPattern = `[0-9]{4}-[0-9]{2}-[0-9]{2} [0-9]{2}:[0-9]{2}:[0-9]{2}`

// awkTimeTokenPattern 是 timeTokenPattern 的 POSIX awk 兼容写法。
// Debian/Ubuntu 默认的 awk 是 mawk，不支持 {n} 区间量词，因此用显式重复，
// 否则 match() 永远不匹配，时间过滤会把所有行都丢掉。
const awkTimeTokenPattern = `[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9] [0-9][0-9]:[0-9][0-9]:[0-9][0-9]`

var timeTokenRe = regexp.MustCompile(timeTokenPattern)

// AssemblePattern 拼装【除时间外】的匹配正则：级别 OR + 内容，用 .* 做 AND。
// 时间范围由命令单独按字符串比较处理，不进入此正则。
//
// 语义（与前端"正则"复选框一致）：
//   - useRegex=true：自定义正则生效（覆盖级别/内容）；否则按内容关键词做字面量匹配。
//   - useRegex=false：普通文本模式，自定义正则被忽略，内容按字面量匹配。
func AssemblePattern(rule config.FilterRule, useRegex bool) string {
	// 仅在"正则"勾选时自定义正则才生效；普通文本模式忽略它，按字面量匹配内容
	if useRegex && strings.TrimSpace(rule.CustomRegex) != "" {
		return rule.CustomRegex
	}
	var parts []string
	if len(rule.Levels) > 0 {
		if lr := levelRegex(rule.Levels); lr != "" {
			parts = append(parts, lr)
		}
	}
	if strings.TrimSpace(rule.Content) != "" {
		parts = append(parts, contentPattern(rule.Content, useRegex))
	}
	return strings.Join(parts, ".*")
}

// TimeBounds 按时间粒度把界面输入规整为时间区间（秒级字符串）。
// 返回 start/end，任一可能为空字符串，表示该侧无界（开区间）。
//
// 语义：
//   - 两端都为空：返回 ("", "", nil)，表示不启用时间过滤；
//   - 输入合法（含单边范围）：返回秒级端点，err 为 nil；
//   - 时间串无法解析或终点早于起点：返回 ("", "", err)，调用方必须把错误
//     返回给用户，而不是悄悄退化成"读取全部日志"。
//
// 支持单边范围：
//   - 只填起点：保留 t >= start 的行
//   - 只填终点：保留 t <= end 的行
//   - 两端都填：闭区间 [start, end]
//
// 粒度对齐（各自独立）：
//   - day：起点补 00:00:00，终点补 23:59:59
//   - hour：起点补 :00:00，终点补 :59:59
//   - minute：起点补 :00，终点补 :59
//   - second：原样（输入已到秒）
func TimeBounds(rule config.FilterRule) (start string, end string, err error) {
	sStr := strings.TrimSpace(rule.TimeStart)
	eStr := strings.TrimSpace(rule.TimeEnd)
	if sStr == "" && eStr == "" {
		return "", "", nil
	}
	prec := rule.TimePrecision
	if prec == "" {
		prec = "second"
	}
	var startT, endT time.Time
	var hasStart, hasEnd bool
	if sStr != "" {
		t, parseOK := parseFlexibleTime(sStr)
		if !parseOK {
			return "", "", fmt.Errorf("开始时间格式无法识别: %q", sStr)
		}
		startT = snapBound(t, prec, false)
		hasStart = true
	}
	if eStr != "" {
		t, parseOK := parseFlexibleTime(eStr)
		if !parseOK {
			return "", "", fmt.Errorf("结束时间格式无法识别: %q", eStr)
		}
		endT = snapBound(t, prec, true)
		hasEnd = true
	}
	// 两端都填且终点早于起点：非法区间。
	if hasStart && hasEnd && endT.Before(startT) {
		return "", "", errors.New("结束时间不能早于开始时间")
	}
	if hasStart {
		start = startT.Format(timeLayout)
	}
	if hasEnd {
		end = endT.Format(timeLayout)
	}
	return start, end, nil
}

// snapBound 按粒度对齐单个端点：起点向下取整到段首，终点向上取整到段尾。
func snapBound(t time.Time, precision string, isEnd bool) time.Time {
	y, mo, d := t.Date()
	switch precision {
	case "day":
		if isEnd {
			return time.Date(y, mo, d, 23, 59, 59, 0, time.Local)
		}
		return time.Date(y, mo, d, 0, 0, 0, 0, time.Local)
	case "hour":
		if isEnd {
			return time.Date(y, mo, d, t.Hour(), 59, 59, 0, time.Local)
		}
		return time.Date(y, mo, d, t.Hour(), 0, 0, 0, time.Local)
	case "minute":
		if isEnd {
			return time.Date(y, mo, d, t.Hour(), t.Minute(), 59, 0, time.Local)
		}
		return time.Date(y, mo, d, t.Hour(), t.Minute(), 0, 0, time.Local)
	}
	return t
}

func levelRegex(levels []string) string {
	var parts []string
	for _, l := range levels {
		if l = strings.TrimSpace(l); l != "" {
			parts = append(parts, regexp.QuoteMeta(l))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	// 用普通捕获分组而非 (?:...) 非捕获组：后者是 PCRE 语法，
	// POSIX ERE（grep -E）不支持，会导致 Linux 下级别正则完全不匹配。
	return "(" + strings.Join(parts, "|") + ")"
}

func contentPattern(content string, useRegex bool) string {
	content = strings.TrimSpace(content)
	if content == "" {
		return ""
	}
	if useRegex {
		return content
	}
	return regexp.QuoteMeta(content)
}

// ===== 时间解析与粒度对齐 =====

const timeLayout = "2006-01-02 15:04:05"

// parseFlexibleTime 宽松解析，支持到日/时/分/秒。
func parseFlexibleTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(strings.ReplaceAll(s, "T", " "))
	if s == "" {
		return time.Time{}, false
	}
	for _, l := range []string{
		"2006-01-02 15:04:05",
		"2006-01-02 15:04",
		"2006-01-02 15",
		"2006-01-02",
	} {
		if t, err := time.ParseInLocation(l, s, time.Local); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

