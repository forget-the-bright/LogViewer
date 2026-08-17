package cmdbuild

import (
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
)

// localPowerShell 是本机执行 PowerShell 命令时使用的可执行文件路径。
// 优先使用 pwsh（PowerShell 7+，基于 .NET Core，启动比 5.1 快约 5 倍），
// 未安装时回退到系统自带的 powershell（5.1）。只影响本机执行，
// SSH 远程路径始终用 "powershell"（远端不一定装了 pwsh）。
var localPowerShell = detectLocalPowerShell()

func detectLocalPowerShell() string {
	if p, err := exec.LookPath("pwsh"); err == nil && p != "" {
		return p
	}
	return "powershell"
}

// LocalPowerShell 返回本机执行 PowerShell 命令时实际使用的可执行文件路径
// （pwsh 7+ 的完整路径，或回退的 "powershell"）。供启动日志/诊断展示。
func LocalPowerShell() string {
	return localPowerShell
}

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
// Windows 下优先用 pwsh（PowerShell 7+，启动更快），未安装时回退 powershell 5.1。
func (c Command) BuildCmd() *exec.Cmd {
	if c.Shell == "powershell" {
		slog.Info("执行本地命令", "shell", localPowerShell)
		return exec.Command(localPowerShell, "-NoProfile", "-NonInteractive", "-WindowStyle", "Hidden", "-Command", c.Script)
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

// BuildRegexCheck 构造一条"只做正则语法校验、不读取任何文件"的命令。
//
// 设计原因：正则最终由目标机器的原生引擎执行（Unix: grep -E 的 POSIX ERE；
// Windows: .NET 正则），与 Go 的 RE2 语法不同（RE2 支持 (?:...)、\d、\s 等，
// POSIX ERE 不支持；反过来 .NET 又支持回溯/反向引用）。用 Go 的 regexp 编译
// 会既误判合法、又漏判非法。根治做法是把用户的正则交给【真正会执行它的引擎】
// 做空跑语法检查：喂空输入，让引擎在处理数据前先编译模式，语法错误会立即以
// 非零退出 + stderr 报出，合法则静默成功。
//
// fixedExclude 为 true 时排除串按字面量（grep -F / SimpleMatch）校验，
// 因为非正则模式下排除串不是正则。
//
// 这些命令不打开任何日志文件，执行代价是一次进程/会话创建，耗时可忽略。
func BuildRegexCheck(platform, pattern string, caseSensitive bool) Command {
	if platform == "windows" {
		// .NET: 构造 Regex 对象即编译模式；喂空输入管道避免 Select-String 等待 stdin。
		// 用 try/catch 把 [ArgumentException] 转成干净的中文错误并以非零退出。
		opts := "None"
		if !caseSensitive {
			opts = "IgnoreCase"
		}
		// 强制 UTF-8 输出，否则本机中文 Windows 下异常消息会是 GBK 字节，前端乱码。
		script := "[Console]::OutputEncoding=[System.Text.Encoding]::UTF8; " +
			"$OutputEncoding=[System.Text.Encoding]::UTF8; " +
			"try { [void][regex]::new(" + psQuote(pattern) + ",[Text.RegularExpressions.RegexOptions]::" + opts + "); exit 0 } " +
			"catch [ArgumentException] { [Console]::Error.WriteLine('正则语法错误: ' + $_.Exception.Message); exit 2 }"
		return Command{Platform: platform, Shell: "powershell", Script: script}
	}
	// Unix: grep -E 在读取输入前就会编译 pattern。以 /dev/null 作空输入。
	// 关键：grep 退出码语义是 0=匹配、1=无匹配、2=语法/IO 错误。对空输入而言，
	// 【合法】正则必然返回 1（无匹配），若直接用退出码判断会把所有合法正则误判为非法。
	// 因此用 `test $? -lt 2` 归一化：0/1（合法）→ 退出 0；2（错误）→ 退出非 0。
	// stderr 仍原样透出，供上层提取引擎错误信息。
	var o []string
	o = append(o, "-E")
	if !caseSensitive {
		o = append(o, "-i")
	}
	inner := "grep " + strings.Join(o, " ") + " " + shQuote(pattern) + " /dev/null"
	script := inner + "; test $? -lt 2"
	return Command{Platform: platform, Shell: "sh", Script: script}
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
	if IsGBK(encoding) {
		cmd += " | iconv -f GBK -t UTF-8"
	}
	// 时间范围：awk 字符串比较（有状态：保留落在范围内时间戳行之后的无时间戳续行，如堆栈）
	// 支持单边范围：任一端非空即启用过滤，空端表示无界。
	if f.TimeStart != "" || f.TimeEnd != "" {
		cmd += " | " + unixTimeStage(f.TimeStart, f.TimeEnd, mode == "follow")
	}
	cmd += unixFilter(f, mode == "follow")
	return cmd
}

// unixTimeStage 用 awk 做时间范围过滤。
// 匹配到时间戳的行按字典序比较；未带时间戳的行（堆栈续行）沿用上一行的判定。
// start/end 任一可为空，表示该侧无界（开区间）。
func unixTimeStage(start, end string, lineBuffered bool) string {
	// 动态拼比较条件：空端不参与比较。用 -v 传参，避免注入；fflush 保证 follow 实时输出。
	// 注意：正则必须用 awk 兼容写法（awkTimeTokenPattern），mawk 不支持 {n} 区间量词。
	var cond string
	switch {
	case start != "" && end != "":
		cond = `t>=s && t<=e`
	case start != "":
		cond = `t>=s`
	default:
		cond = `t<=e`
	}
	prog := `{ if (match($0, /` + awkTimeTokenPattern + `/)) { t=substr($0,RSTART,RLENGTH); keep=(` + cond + `) } if (keep) print`
	if lineBuffered {
		prog += `; fflush()`
	}
	prog += ` }`
	args := "awk "
	if start != "" {
		args += "-v s=" + shQuote(start) + " "
	}
	if end != "" {
		args += "-v e=" + shQuote(end) + " "
	}
	return args + shQuote(prog)
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
		windowsFollowStage(&sb, filePath, encoding, limit)
	} else {
		windowsStaticStage(&sb, filePath, encoding, limit)
	}
	if f.TimeStart != "" || f.TimeEnd != "" {
		sb.WriteString(windowsTimeStage(f.TimeStart, f.TimeEnd))
	}
	sb.WriteString(windowsFilter(f))
	return sb.String()
}

// windowsFollowStage 构造 Windows 实时跟踪阶段。
//
// UTF-8 直接用 Get-Content -Wait -Tail/-Encoding UTF8（原生、高效）。
//
// GBK 必须兼容两个 PowerShell 版本（详见 gbkReadStage 文档）：
//   - PS 7+：-Encoding 接受 Encoding 对象，直接传 GetEncoding(936) 显式 GBK，
//     纯原生零开销，且与系统区域无关。
//   - PS 5.1：-Encoding 只认枚举，用 GetEncoding(0).CodePage（真实系统 ACP）分流，
//     ACP=936 时 -Encoding Default 原生零开销，非 936 才逐行转码。
//
// 关键：快路径分支绝不能套 ForEach-Object，否则每行都过一次 PowerShell 管道，
// 带过滤时会明显拖慢（实测尾部 200 行从 ~180ms 退化到 ~600ms）。
func windowsFollowStage(sb *strings.Builder, filePath, encoding string, limit int) {
	if !IsGBK(encoding) {
		// UTF-8：原生文本跟踪
		gc := "-LiteralPath " + psQuote(filePath) + " -Encoding UTF8"
		if limit > 0 {
			sb.WriteString("& { Get-Content " + gc + " -Wait -Tail " + strconv.Itoa(limit) + " }")
		} else {
			sb.WriteString("Get-Content " + gc + " -Wait")
		}
		return
	}

	// GBK：Get-Content -Wait -Tail N（原生尾部定位+跟踪），
	// 编码由 gbkReadStage 按 PS 版本/系统 ACP 自适应。
	gc := "-LiteralPath " + psQuote(filePath) + " -Wait"
	if limit > 0 {
		gc += " -Tail " + strconv.Itoa(limit)
	}
	sb.WriteString(gbkReadStage(gc))
}

// windowsStaticStage 构造 Windows 静态加载阶段。
//
// UTF-8 有 -Tail 时用 Get-Content -Tail（原生尾部定位，最快），
// 无 -Tail 时用 [IO.File]::ReadLines（比 Get-Content 快约 3 倍）。
//
// GBK 有 -Tail 时走 gbkReadStage（PS 版本自适应，原生尾部定位，最快），
// 无 -Tail 时直接 [IO.File]::ReadLines(path,GBK)，显式按 GBK 解码，
// 跨版本/跨区域正确且不经 Default。
func windowsStaticStage(sb *strings.Builder, filePath, encoding string, limit int) {
	if !IsGBK(encoding) {
		if limit > 0 {
			sb.WriteString("Get-Content -LiteralPath " + psQuote(filePath) + " -Encoding UTF8 -Tail " + strconv.Itoa(limit))
		} else {
			sb.WriteString("[IO.File]::ReadLines(" + psQuote(filePath) + ",[Text.Encoding]::UTF8)")
		}
		return
	}

	// GBK
	if limit > 0 {
		// Get-Content -Tail 原生从文件尾部定位，比 ReadLines 全量枚举快一个数量级。
		sb.WriteString(gbkReadStage("-LiteralPath " + psQuote(filePath) + " -Tail " + strconv.Itoa(limit)))
	} else {
		sb.WriteString("[IO.File]::ReadLines(" + psQuote(filePath) + ",[Text.Encoding]::GetEncoding('GBK'))")
	}
}

// gbkReadStage 包装一次 GBK 文件读取，在 PowerShell 5.1 与 7+ 上都能正确解码。
//
// baseArgs 是 Get-Content 的参数串，【不含】 -Encoding（例如
// "-LiteralPath 'C:\x.log' -Wait -Tail 100"），编码由本阶段按运行时环境添加：
//
//   - PS 7+：-Encoding 接受 Encoding 对象；但 [Text.Encoding]::Default 在 PS 7
//     下恒为 UTF-8（不再是系统 ACP），-Encoding Default 会把 GBK 字节当 UTF-8
//     解码而乱码。因此直接传 -Encoding ([Text.Encoding]::GetEncoding(936))，
//     显式按 GBK 读取，纯原生、零转码开销，且与系统区域无关。
//   - PS 5.1 中文 Windows（ACP=936）：-Encoding Default 即 GBK，原生零开销快路径。
//   - PS 5.1 非中文 Windows（ACP≠936）：-Encoding Default 按本地 ACP 读取，
//     再逐行转码为 GBK。
//
// 系统 ACP 用 [Text.Encoding]::GetEncoding(0) 获取——两个 PS 版本下都返回真实
// 系统代码页（不能用 [Text.Encoding]::Default，它在 PS 7 下固定为 UTF-8）。
//
// 无损性：Get-Content 按行（0x0A/0x0D 分割）产出字符串，而 GBK 尾字节范围
// 是 0x40-0xFE（排除 0x7F），永不包含换行符，按行绝不会切断多字节字符。
func gbkReadStage(baseArgs string) string {
	return "& { " +
		"if ($PSVersionTable.PSVersion.Major -ge 7) { " +
		"Get-Content " + baseArgs + " -Encoding ([Text.Encoding]::GetEncoding(936)) " +
		"} elseif ([Text.Encoding]::GetEncoding(0).CodePage -eq 936) { " +
		"Get-Content " + baseArgs + " -Encoding Default " +
		"} else { " +
		"$lv_g=[Text.Encoding]::GetEncoding('GBK'); $lv_d=[Text.Encoding]::GetEncoding(0); " +
		"Get-Content " + baseArgs + " -Encoding Default | ForEach-Object { $lv_g.GetString($lv_d.GetBytes($_)) } } }"
}

// windowsTimeStage 用 Where-Object 做时间范围字符串比较。
// $script:_keep 跨管道对象保持状态，使无时间戳的堆栈续行跟随上一条时间戳行的判定。
// start/end 任一可为空，表示该侧无界。
func windowsTimeStage(start, end string) string {
	var cmp string
	switch {
	case start != "" && end != "":
		cmp = "$t -ge " + psQuote(start) + " -and $t -le " + psQuote(end)
	case start != "":
		cmp = "$t -ge " + psQuote(start)
	default:
		cmp = "$t -le " + psQuote(end)
	}
	return " | Where-Object { if ($_ -match '" + timeTokenPattern + "') { $t=$Matches[0]; $script:_keep=(" + cmp + ") }; $script:_keep }"
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

// IsGBK 报告编码名是否为 GBK / GB2312（大小写、前后空白不敏感）。
// 唯一判定入口：server 与 cmdbuild 共用，避免两处实现不一致导致
// checkCaps 判定需要 iconv 而命令构建却没加转码阶段的错配。
func IsGBK(enc string) bool {
	enc = strings.ToLower(strings.TrimSpace(enc))
	return enc == "gbk" || enc == "gb2312"
}

// ---- 转义 ----

func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func psQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
