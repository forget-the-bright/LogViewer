package host

import "strings"

// defaultLogExts 是未显式配置 file_extensions 时展示的日志文件后缀。
var defaultLogExts = []string{".log", ".out"}

// normalizeExts 把配置中的后缀列表规整为小写、带点的集合。
//
// 规则：
//   - 去除空白；自动补全前导点（"txt" -> ".txt"）；
//   - 含 "*" 时返回 showAll=true，表示目录树展示所有文件（不过滤后缀）；
//   - 空列表回退到默认 .log/.out。
//
// 目录始终展示，不受此后缀集合影响。
func normalizeExts(list []string) (set map[string]bool, showAll bool) {
	set = map[string]bool{}
	for _, e := range list {
		e = strings.TrimSpace(strings.ToLower(e))
		if e == "" {
			continue
		}
		if e == "*" {
			return nil, true
		}
		if !strings.HasPrefix(e, ".") {
			e = "." + e
		}
		set[e] = true
	}
	if len(set) == 0 {
		for _, e := range defaultLogExts {
			set[e] = true
		}
	}
	return set, false
}

// extAllow 判断给定文件名（按其后缀）是否应在目录树中展示。
// showAll 为 true 时放行所有文件；否则要求后缀命中集合。无后缀文件在非 showAll 下被过滤。
func extAllow(name string, set map[string]bool, showAll bool) bool {
	if showAll {
		return true
	}
	dot := strings.LastIndex(name, ".")
	if dot < 0 {
		return false
	}
	return set[strings.ToLower(name[dot:])]
}
