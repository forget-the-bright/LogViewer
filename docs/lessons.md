# 经验总结 / 踩坑记录

记录开发过程中真实遇到的问题、错误的排查方向和最终结论。每条都尽量给出**根因**，
而不是停留在"这么改就好了"。

---

## Linux 下时间过滤 / 级别正则不生效

**现象**：同样的配置在 Windows 正常，Linux 上勾选时间范围或日志级别后没有任何输出。

**根因（两处正则方言不兼容）**：

1. **时间阶段用的 awk 正则含 `{n}` 区间量词**。Debian/Ubuntu 默认的 awk 是 mawk，
   它不支持 `{4}`/`{2}` 这类区间表达式（gawk 旧版还需 `--re-interval`），于是
   `match($0, /[0-9]{4}-.../)` 永远不匹配，`keep` 恒为 false，所有行被丢弃。
2. **级别正则用了 PCRE 非捕获组 `(?:ERROR|WARN)`**。Linux 用 `grep -E`，遵循
   POSIX ERE，ERE **没有 `(?:...)` 语法**，整条正则匹配失败；而 Windows 的
   `Select-String` 用 .NET 正则支持，所以只在 Linux 坏。

**修复**：

- awk 时间戳正则改成显式重复：`[0-9][0-9][0-9][0-9]-[0-9][0-9]-...`，所有 POSIX awk 通用。
- 级别分组从 `(?:...)` 改成普通捕获组 `(...)`，ERE 和 .NET 都支持，捕获无副作用。
- Go 的 `regexp` 和 PowerShell 不受影响，仍可用 `{n}`。

**教训**：跨平台拼原生命令的正则时，必须取 grep ERE / awk / .NET 的**公共子集**，
不能想当然地用 PCRE 语法。排查时把后端打印的实际命令粘到目标系统 shell 里直接跑，
一秒定位。为此在 `ws.go` / `export.go` 启动命令前加了 `log.Printf` 打印完整脚本。

---

## Linux 实时跟踪不指定行数时"一直不返回"

**现象**：实时跟踪指定行数能看到末 N 行，不指定行数时终端一直空白，像在处理中。

**根因**：Unix 侧用的是 `tail -F <file>`（不带 `-n`）。各 `tail` 实现不带 `-n` 时
默认行为不一致——有的不回显历史只等新增，有的只给末尾 10 行，与"先看全量再跟随"
的预期不符。

**修复**：与 Windows 两步式对齐，不指定行数时用分组命令
`{ cat <file>; tail -F -n 0 <file>; }`：先 `cat` 吐全部已有内容，再
`tail -F -n 0` 只跟随新增。`{ ...; }` 让两者 stdout 汇入同一条过滤管道。
指定行数时仍是一步 `tail -F -n N`。

> 已知边界：`cat` 读到 EOF 与 `tail -F -n 0` 启动之间有极小的竞态窗口，期间追加的
> 行理论上可能漏掉（Windows 的两步式同样存在）。日志查看场景可接受；若要严格无漏读，
> 可改用 `tail -F -n +1`（从第一行开始跟随，取决于 tail 版本支持）。

---

## 实时跟踪"只显示一行，停止时才吐出剩余"

**现象**：实时跟踪选了末 200 行，前端只立即显示其中一行，其余要等到追加新内容
或点停止时才一起冒出来。

**走过的弯路**：

1. 怀疑 PowerShell `Get-Content -Wait` 缓冲了输出，尝试用 `[Console]::Out.WriteLine`
   强制刷新 —— 错，那写的是控制台句柄不是管道。
2. 用 `StreamWriter(AutoFlush=$true)` 包住管道 —— 数据在上游就被缓冲，包不住。
3. 自己写 .NET `FileStream`/`StreamReader` 轮询脚本 —— 能解决，但违背"一切都是命令、
   不写自定义脚本"的原则，被否决。
4. 改成两步原生命令 `Get-Content -Tail N; Get-Content -Wait -Tail 0` —— 命令本身对了，
   但现象依旧。

**真正的根因**：写了一个独立的 Go 测试程序直接 exec PowerShell 命令并打印带时间戳的输出，
证明**所有命令都在 0.3~0.8 秒内把末 N 行全部输出了**，命令没有问题。问题在
`procmgr.readLoop` 的节流逻辑：

```go
// 错误版本：计时器只在 ReadString 返回后才检查
for {
    line, err := br.ReadString('\n')   // 没新数据时一直阻塞
    batch = append(batch, line)
    if len(batch) > 0 && time.Since(lastFlush) >= interval {
        flush()                          // ← 阻塞期间永远到不了这里
    }
}
```

首批历史行里，第一行触发 flush 后，后续行在 40ms 窗口内进了 batch，但随后
`ReadString` 阻塞等待新数据，定时 flush 永远没机会执行，于是这些行卡在 batch 里，
直到下一行到来（追加内容）才被捎带 flush —— 正是"停止/追加时吐出一堆"。

**修复**：用**独立的 ticker 协程**定时 flush，batch 用 mutex 保护，与读取协程解耦：

```go
go func() { for range ticker.C { flush() } }()
for {
    line, _ := br.ReadString('\n')
    mu.Lock(); batch = append(batch, line); full := len(batch) >= max; mu.Unlock()
    if full { flush() }
}
```

**教训**：

- 阻塞式读取上做"定时"不能依赖"读完再看表"，必须有独立的时间源（ticker 协程）。
- 怀疑下游缓冲之前，先用最小复现程序把**命令的真实输出时间戳**打出来，把命令和
  进程管理两层切开验证。不要在没数据的情况下凭直觉改命令。

---

## 时间范围过滤不能用正则枚举

最初想把"时间在 A 到 B 之间"翻译成一个大正则。范围稍大（比如一天，精确到秒）
正则就会膨胀到数万字符，既慢又脆弱。

**结论**：`YYYY-MM-DD HH:MM:SS` 是定长字符串，**字典序 == 时间序**。直接做字符串
比较即可：Unix 用 `awk`，Windows 用 `Where-Object`，命令长度与时间跨度无关。
不带时间戳的续行（Java 堆栈）用一个状态变量沿用上一行的判定。

详见 [native-command-pipeline.md](native-command-pipeline.md#3-时间范围--字符串比较而不是正则枚举)。

---

## xterm.js 行号：不要用前缀文本，用双终端 gutter

**需求**：终端里显示行号。

**错误做法**：在每行内容前拼 `"  12| "` —— 会污染复制内容、和自动换行打架、
高亮着色也对不齐。

**正确做法**：并排放两个 xterm 终端：

- 左侧 `gutter`：只读、`pointer-events:none`、固定列宽，只写行号；
- 右侧 `term`：正常日志终端；
- 监听 `term.onScroll`，用 `term.buffer.active.viewportY` 对 gutter 做
  **绝对**滚动同步（不要用相对行差，长换行下会累积误差）；
- 遍历主 buffer 行时，用 `line.isWrapped` 判断是否为自动换行的续行 ——
  续行不递增逻辑行号、在 gutter 里留空，保证两个终端的物理行一一对应；
- gutter 隐藏后再显示要用 `requestAnimationFrame` 等布局完成再 fit + rebuild，
  否则尺寸算错。xterm 没有原生行号 gutter，这是社区通用方案。

**坑**：行号从 2 开始，是因为 buffer 末尾那个空的光标行被当成了一行；需要跳过
"末尾空白行"。

---

## "反转匹配"在普通与正则模式都生效

历史上前端在勾选"正则"时会隐藏并强制清空"反转"，理由是"反转仅普通模式有意义"。
这个理由不成立：后端 `grepIncludeOpts` 对主模式恒用 `grep -E`，反转只是追加 `-v`
（即 `grep -vE <pattern>`）；Windows 端 `Select-String -NotMatch -Pattern <regex>`
同样支持对正则取反。因此对正则取反是合法且常见的需求。

现已对齐：两种模式都显示并提交 `InvertMatch`。`AssemblePattern(rule, useRegex)`
在普通模式下仍**忽略自定义正则**、内容用 `regexp.QuoteMeta` 转成字面量；
但 `InvertMatch` 不再因模式而被清零。语义必须前后端一致，否则保存的配置重载后会失真。

---

## 过滤导出"导出的是全部而不是过滤结果"

根因：过滤导出接口按"配置名"去读**已保存**的配置，而用户当前在表单里改了条件
但没保存，于是导出的不是当前看到的结果。

修复：过滤导出改成 `POST`，请求体直接带当前表单的完整 `LogConfig`，与 WebSocket
实时跟踪用同一份 `BuildView` 命令；导出文件名加 `_filtered` 后缀区分。

---

## 导出大文件要流式，不要小块 flush

最初为了"进度感"在导出循环里每读 32KB 就 `Flush()`，大文件导出明显变慢。
改成 `io.Copy(c.Writer, stdout)` 让底层自行缓冲；进度条仍可通过
`response.body.getReader()` 在浏览器端按已接收字节数计算，服务端不需要逐块 flush。

原始导出设置 `Content-Length`（`os.Stat` 取大小），浏览器能显示准确的总进度；
过滤导出无法预知大小，进度条显示为"已接收 / 不确定总量"的滚动样式。

---

## Windows 静态读取用 ReadLines 而不是 Get-Content

静态全量模式下 `Get-Content` 逐行产生 PS 对象，大文件慢。改用
`[IO.File]::ReadLines(path, encoding)` 直接 .NET 枚举，速度接近 `cat`。
有 `-Tail N` 需求时仍用 `Get-Content -Tail`（ReadLines 不便高效取尾部）。

---

## 进程一定要连进程组一起杀

Unix 下命令是 `sh -c "tail | grep | awk"`，`Process.Kill` 只杀 `sh`，
`tail`/`grep` 会变成孤儿继续读文件、占用句柄。

- Unix：`SysProcAttr{Setpgid: true}` 让子进程成独立进程组，停止时
  `syscall.Kill(-pid, SIGKILL)` 杀整组。
- Windows：用 `taskkill /T /F /PID` 终止进程树。不能只 `Process.Kill` 顶层
  powershell —— 它可能派生子宿主，`/T` 连同子孙进程一起杀，杜绝僵尸
  powershell / conhost 残留。

**`done` 必须在输出读完后再关闭**：`cmd.Wait()` 不会等待我们自己用 `StdoutPipe`
读的协程。若 Wait 一返回就 close(done)，读取协程可能还在做最后一次 flush，
导致"已经 stop 了还往 WebSocket 写"。正确顺序是：先 `readersWg.Wait()`（等
stdout/stderr 读到 EOF 并完成 flush）→ 再 `cmd.Wait()` → 再 close(done)。
这也符合 os/exec 关于 StdoutPipe 的标准用法（读完管道再 Wait）。

`ticker` 协程要有明确的退出握手：主循环结束时 `close(stopFlush)` 并 `Wait`
flush 协程，否则它可能在主函数返回后仍在途调用 `outFn`。

`Stop` 在 kill 后还要等 `done`（带超时）回收；WebSocket 断开的 `defer`
里必须调 `stopSession`，保证关闭浏览器标签页也不残留进程。

---

## 路径穿越防护

目录浏览和文件读取都接受前端传来的路径。必须校验：`filepath.Abs` →
`filepath.Clean` → 对每个配置的 root 做 `filepath.Rel`，任何 `..` 跳出 root 的
请求一律拒绝。不能只检查字符串前缀（`/root` 前缀能被 `/root-evil/` 绕过）。

---

## 时间选择控件不要用原生 datetime-local 的 step

原生 `<input type="datetime-local" step=...>` 在各浏览器下"粒度限制"行为不一致，
而且样式无法统一。最终用 [flatpickr](https://flatpickr.js.org/)（本地 vendor，
不联网），按天/时/分/秒切 `enableTime` 和 `dateFormat`，切换粒度时销毁重建实例
并保留已选日期。

---

## 前端资源全部本地 vendor

xterm.js、flatpickr 都放在 `static/vendor/`，随 `go:embed` 进二进制。好处：

- 单文件部署，目标机器无需联网、无需 npm；
- 不会因为 CDN 不可用而白屏；
- 版本锁定，构建可复现。

---

## 通用排查方法论

1. **分层切开**：命令层（直接在终端跑）、进程管理层（独立 exec 测试程序）、
   WebSocket 层（DevTools 看帧）、前端层（xterm）。先确定是哪一层的问题，
   不要跨层瞎改。
2. **打时间戳**：怀疑缓冲/延迟问题时，在测试程序里给每批输出打相对时间戳，
   能立刻分辨是"命令没吐"还是"吐了但没转发"。
3. **最小复现**：把可疑行为抽成十几行的独立程序（例如单独 exec 一条 PowerShell
   命令并给输出打时间戳），定位后再删除，不留在产品代码里。
4. **尊重原生命令**：先 `man` / `Get-Help` 确认工具本身的能力和缓冲语义，
   再决定是否需要外壳介入。
