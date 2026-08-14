package cmdbuild

import (
	"os/exec"
	"strconv"
	"strings"
)

// Command 描述一次要启动的原生命令管道。
// 设计原则：Go 只做"外壳"，所有日志读取与过滤都由系统原生命令完成
// （Linux/macOS: tail/cat + awk + grep；Windows: Get-Content + Where-Object + Select-String）。
// 时间范围用字符串比较（awk/PowerShell），不用正则枚举，命令长度恒定。
//
// Platform 为目标平台 "linux"/"darwin"/"windows"，决定用 sh 还是 powershell。
// 本机执行时取 runtime.GOOS；远程 SSH 执行时取远程探测到的平台，因此不能在
// 构建期写死 runtime.GOOS。
type Command struct {
	Platform string
	Shell    string // "sh" 或 "powershell"
	Script   string // 命令管道脚本
}

// FilterCfg 过滤参数（由前端 LogConfig 映射而来）。
// Pattern 为级别+内容拼装出的正则；TimeStart/TimeEnd 为秒级闭区间（独立阶段处理）。
type FilterCfg struct {
	Pattern       string // 匹配模式（正则，空=不按内容过滤）
	Exclude       string // 排除模式（正则或字面量，独立反转过滤）
	TimeStart     string // 时间起 "YYYY-MM-DD HH:MM:SS"（空=不按时间过滤）
	TimeEnd       string // 时间止
	UseRegex      bool   // 内容/排除是否按正则
	CaseSensitive bool
	InvertMatch   bool // 反转匹配（grep -v / -NotMatch），作用于主模式
	ContextBefore int  // 匹配行前 N 行
	ContextAfter  int  // 匹配行后 N 行
}

// BuildCmd 将 Command 转成 *exec.Cmd（本机执行用）。
func (c Command) BuildCmd() *exec.Cmd {
	if c.Shell == "powershell" {
		return exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", c.Script)
	}
	return exec.Command("sh", "-c", c.Script)
}

// BuildView 构造一次"查看"命令（静态加载或实时跟踪）。
// platform 为目标平台："linux"/"darwin" 走 Unix 命令，"windows" 走 PowerShell。
func BuildView(platform, mode, filePath, encoding string, limit int, f FilterCfg) Command {
	if platform == "windows" {
		return Command{Platform: platform, Shell: "powershell", Script: windowsView(mode, filePath, encoding, limit, f)}
	}
	return Command{Platform: platform, Shell: "sh", Script: unixView(mode, filePath, encoding, limit, f)}
}

// BuildExport 构造"导出过滤日志"命令。
func BuildExport(platform, filePath, encoding string, limit int, f FilterCfg) Command {
	return BuildView(platform, "static", filePath, encoding, limit, f)
}

// ---- Unix (Linux/macOS) ----

func unixView(mode, filePath, encoding string, limit int, f FilterCfg) string {
	fq := shQuote(filePath)
	var base string
	if mode == "follow" {
		if limit > 0 {
			// 指定行数：等价 tail -n N -F
			base = "tail -F -n " + strconv.Itoa(limit) + " " + fq
		} else {
			// 不指定行数：先 cat 输出全部已有内容，再 tail -F -n 0 只跟随新增。
			// tail -F 不带 -n 时各实现默认行数不一致（有的不回显历史，有的只给末尾 10 行），
			// 用 cat + tail -F -n 0 两步式保证"先看全量再实时追加"，与 Windows 行为一致。
			// 用 { ...; } 分组，使两者的 stdout 汇入后面同一条过滤管道。
			base = "{ cat " + fq + "; tail -F -n 0 " + fq + "; }"
		}
	} else {
		if limit > 0 {
			base = "tail -n " + strconv.Itoa(limit) + " " + fq
		} else {
			base = "cat " + fq
		}
	}
	cmd := base
	if isGBK(encoding) {
		cmd += " | iconv -f GBK -t UTF-8"
	}
	// 时间范围：awk 字符串比较（有状态：保留落在范围内时间戳行之后的无时间戳续行，如堆栈）
	if f.TimeStart != "" && f.TimeEnd != "" {
		cmd += " | " + unixTimeStage(f.TimeStart, f.TimeEnd, mode == "follow")
	}
	cmd += unixFilter(f, mode == "follow")
	return cmd
}

// unixTimeStage 用 awk 做时间范围过滤。
// 匹配到时间戳的行按字典序比较；未带时间戳的行（堆栈续行）沿用上一行的判定。
func unixTimeStage(start, end string, lineBuffered bool) string {
	// 用 -v 传参，避免注入；fflush 保证 follow 模式实时输出。
	// 注意：正则必须用 awk 兼容写法（awkTimeTokenPattern），mawk 不支持 {n} 区间量词。
	prog := `{ if (match($0, /` + awkTimeTokenPattern + `/)) { t=substr($0,RSTART,RLENGTH); keep=(t>=s && t<=e) } if (keep) print`
	if lineBuffered {
		prog += `; fflush()`
	}
	prog += ` }`
	return "awk -v s=" + shQuote(start) + " -v e=" + shQuote(end) + " " + shQuote(prog)
}

// unixFilter 追加 grep 过滤阶段。
func unixFilter(f FilterCfg, lineBuffered bool) string {
	var stages []string
	if f.Pattern != "" {
		stages = append(stages, "grep "+grepIncludeOpts(f, lineBuffered)+shQuote(f.Pattern))
	}
	if f.Exclude != "" {
		stages = append(stages, "grep -v "+grepExclOpts(f, lineBuffered)+shQuote(f.Exclude))
	}
	if len(stages) == 0 {
		return ""
	}
	return " | " + strings.Join(stages, " | ")
}

func grepIncludeOpts(f FilterCfg, lineBuffered bool) string {
	var o []string
	o = append(o, "-E") // 拼装出的 Pattern 一定是正则
	if !f.CaseSensitive {
		o = append(o, "-i")
	}
	if f.InvertMatch {
		o = append(o, "-v")
	}
	if f.ContextBefore > 0 {
		o = append(o, "-B "+strconv.Itoa(f.ContextBefore))
	}
	if f.ContextAfter > 0 {
		o = append(o, "-A "+strconv.Itoa(f.ContextAfter))
	}
	if lineBuffered {
		o = append(o, "--line-buffered")
	}
	return strings.Join(o, " ") + " "
}

func grepExclOpts(f FilterCfg, lineBuffered bool) string {
	var o []string
	if f.UseRegex {
		o = append(o, "-E")
	} else {
		o = append(o, "-F")
	}
	if !f.CaseSensitive {
		o = append(o, "-i")
	}
	if lineBuffered {
		o = append(o, "--line-buffered")
	}
	return strings.Join(o, " ") + " "
}

// ---- Windows PowerShell ----

func windowsView(mode, filePath, encoding string, limit int, f FilterCfg) string {
	var sb strings.Builder
	sb.WriteString("[Console]::OutputEncoding=[System.Text.Encoding]::UTF8; $OutputEncoding=[System.Text.Encoding]::UTF8; ")
	if mode == "follow" {
		// 实时跟踪（两条原生命令顺序执行，不写自定义脚本）：
		//   1) Get-Content -Tail N：一次性读出末尾 N 行，立即输出
		//   2) Get-Content -Wait -Tail 0：从文件末尾开始跟随新增（-Tail 0 = 不回显已有行）
		// 两者输出汇入同一条过滤管道。
		gcFile := "-LiteralPath " + psQuote(filePath) + " -Encoding " + psEncoding(encoding)
		if limit > 0 {
			sb.WriteString("& { Get-Content " + gcFile + " -Wait -Tail " + strconv.Itoa(limit) +
				" }")
		} else {
			sb.WriteString("Get-Content " + gcFile + " -Wait")
		}
	} else {
		// 静态模式：用 .NET ReadLines 逐行枚举，比 Get-Content 快很多；
		// 有 -Tail 行数限制时回退到 Get-Content -Tail（ReadLines 不便高效取尾部）。
		if limit > 0 {
			sb.WriteString("Get-Content -LiteralPath " + psQuote(filePath) + " -Encoding " + psEncoding(encoding) + " -Tail " + strconv.Itoa(limit))
		} else {
			enc := "UTF8"
			if isGBK(encoding) {
				enc = "[Text.Encoding]::GetEncoding('GBK')"
			}
			sb.WriteString("[IO.File]::ReadLines(" + psQuote(filePath) + ",[Text.Encoding]::" + enc + ")")
		}
	}
	if f.TimeStart != "" && f.TimeEnd != "" {
		sb.WriteString(windowsTimeStage(f.TimeStart, f.TimeEnd))
	}
	sb.WriteString(windowsFilter(f))
	return sb.String()
}

// windowsTimeStage 用 Where-Object 做时间范围字符串比较。
// $script:_keep 跨管道对象保持状态，使无时间戳的堆栈续行跟随上一条时间戳行的判定。
func windowsTimeStage(start, end string) string {
	return " | Where-Object { if ($_ -match '" + timeTokenPattern + "') { $t=$Matches[0]; $script:_keep=($t -ge " + psQuote(start) + " -and $t -le " + psQuote(end) + ") }; $script:_keep }"
}

// windowsFilter 追加 Select-String 过滤阶段。
func windowsFilter(f FilterCfg) string {
	var stages []string
	if f.Pattern != "" {
		stages = append(stages, "Select-String "+psIncludeOpts(f)+"-Pattern "+psQuote(f.Pattern))
	}
	if f.Exclude != "" {
		stages = append(stages, "Select-String -NotMatch "+psExclOpts(f)+"-Pattern "+psQuote(f.Exclude))
	}
	if len(stages) == 0 {
		return ""
	}
	extract := "$_.Line"
	if f.ContextBefore > 0 || f.ContextAfter > 0 {
		extract = "@($_.Context.PreContext)+@($_.Line)+@($_.Context.PostContext)"
	}
	return " | " + strings.Join(stages, " | ") + " | ForEach-Object { " + extract + " }"
}

func psIncludeOpts(f FilterCfg) string {
	var o []string
	if f.CaseSensitive {
		o = append(o, "-CaseSensitive")
	}
	if f.InvertMatch {
		o = append(o, "-NotMatch")
	}
	if f.ContextBefore > 0 || f.ContextAfter > 0 {
		o = append(o, "-Context "+strconv.Itoa(f.ContextBefore)+","+strconv.Itoa(f.ContextAfter))
	}
	if len(o) == 0 {
		return ""
	}
	return strings.Join(o, " ") + " "
}

func psExclOpts(f FilterCfg) string {
	var o []string
	if !f.UseRegex {
		o = append(o, "-SimpleMatch")
	}
	if f.CaseSensitive {
		o = append(o, "-CaseSensitive")
	}
	if len(o) == 0 {
		return ""
	}
	return strings.Join(o, " ") + " "
}

func psEncoding(enc string) string {
	if isGBK(enc) {
		return "OEM"
	}
	return "UTF8"
}

func isGBK(enc string) bool {
	return strings.EqualFold(enc, "gbk") || strings.EqualFold(enc, "gb2312")
}

// ---- 转义 ----

func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func psQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
