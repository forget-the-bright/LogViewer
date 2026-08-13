package cmdbuild

import (
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

// TimeBounds 按时间粒度把界面输入规整为闭区间 [start, end]（秒级字符串）。
// 返回 ("","") 表示未设置时间过滤。
//   - day：起点补 00:00:00，终点补 23:59:59
//   - hour：起点补 :00:00，终点补 :59:59
//   - minute：起点补 :00，终点补 :59
//   - second：原样（输入已到秒）
//
// 端点支持只填到日/时/分，按粒度补齐。
func TimeBounds(rule config.FilterRule) (string, string, bool) {
	if strings.TrimSpace(rule.TimeStart) == "" && strings.TrimSpace(rule.TimeEnd) == "" {
		return "", "", false
	}
	prec := rule.TimePrecision
	if prec == "" {
		prec = "second"
	}
	start, ok1 := parseFlexibleTime(rule.TimeStart)
	end, ok2 := parseFlexibleTime(rule.TimeEnd)
	if !ok1 || !ok2 {
		return "", "", false
	}
	start, end = snapBounds(start, end, prec)
	if end.Before(start) {
		return "", "", false
	}
	return start.Format(timeLayout), end.Format(timeLayout), true
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
	return "(?:" + strings.Join(parts, "|") + ")"
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

// snapBounds 按粒度把 start 向下取整、end 向上取整到整段。
func snapBounds(start, end time.Time, precision string) (time.Time, time.Time) {
	y, mo, d := start.Date()
	switch precision {
	case "day":
		start = time.Date(y, mo, d, 0, 0, 0, 0, time.Local)
		ey, emo, ed := end.Date()
		end = time.Date(ey, emo, ed, 23, 59, 59, 0, time.Local)
	case "hour":
		start = time.Date(y, mo, d, start.Hour(), 0, 0, 0, time.Local)
		ey, emo, ed := end.Date()
		end = time.Date(ey, emo, ed, end.Hour(), 59, 59, 0, time.Local)
	case "minute":
		start = time.Date(y, mo, d, start.Hour(), start.Minute(), 0, 0, time.Local)
		ey, emo, ed := end.Date()
		end = time.Date(ey, emo, ed, end.Hour(), end.Minute(), 59, 0, time.Local)
	}
	return start, end
}
