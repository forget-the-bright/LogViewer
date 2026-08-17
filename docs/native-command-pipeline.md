# 原生命令管道设计

本文档说明"Go 只是外壳"原则下，各类配置是如何映射成 Unix / Windows 原生命令的。
所有命令都在 `internal/cmdbuild/cmdbuild.go` 中构建。

## 1. 读取：跟踪与静态

### Unix

- 跟踪且指定行数 N：`tail -F -n N <file>`（`-F` 跟 inode，文件被轮转/重建后自动重连）
- 跟踪且不指定行数：**两步式**
  ```sh
  { cat <file>; tail -F -n 0 <file>; }
  ```
  先 `cat` 输出全部已有内容，再 `tail -F -n 0` 只跟随新增。
  不能直接 `tail -F <file>`：不带 `-n` 时各实现默认行数不一致（有的不回显历史、
  有的只给末尾 10 行），会出现"跟踪但一直不返回历史"的现象。用 `{ ...; }` 分组，
  两条命令的 stdout 汇入后面同一条过滤管道。
- 静态全量：`cat <file>`
- 静态末 N 行：`tail -n N <file>`

> 停止跟踪时 Unix 用独立进程组 `Kill(-pgid, SIGKILL)`，`cat`/`tail`/`grep` 整条管道一起杀，不留残留。

### Windows

- 跟踪且有 N：`Get-Content -LiteralPath <file> -Encoding UTF8 -Wait -Tail N`。
  `Get-Content -Wait -Tail N` 本身就会先输出末 N 行再持续跟随新增，单条命令即可，
  不需要分两段。
- 跟踪无 N：`Get-Content -Wait`（从文件开头读出已有内容后持续跟随）。
- 静态全量：`[IO.File]::ReadLines(<file>, [Text.Encoding]::UTF8)`（比 `Get-Content` 快）
- 静态末 N 行：`Get-Content -Tail N`

开头强制设置 `[Console]::OutputEncoding=UTF8`，保证管道里是 UTF-8 字节，Go 端无需再转码。

## 2. 编码（GBK）

- Unix：在读取命令后接 `iconv -f GBK -t UTF-8`。
- Windows：用**运行时代码页分流**，避免在中文系统上为转码付出逐行开销：
  ```powershell
  if ([Text.Encoding]::Default.CodePage -eq 936) {
      Get-Content ... -Encoding Default ...      # 中文系统：ANSI=936 即 GBK，纯原生零开销
  } else {
      $lv_g=[Text.Encoding]::GetEncoding('GBK'); $lv_d=[Text.Encoding]::Default
      Get-Content ... -Encoding Default ... |
        ForEach-Object { $lv_g.GetString($lv_d.GetBytes($_)) }   # 非中文系统：逐行转码
  }
  ```
  - 中文系统（代码页 936）`-Encoding Default` 直接读出 GBK 文本并经控制台 UTF-8 输出，
    走原生 `-Tail/-Wait` 尾部定位，不引入逐行管道开销；
  - 非中文系统（如英文 Windows 的 1252/437/850）`Default` 不是 GBK，才把读出的字符串
    按 Default 编码反解为字节、再用 `GetEncoding('GBK')` 解码为正确文本；
  - 静态全量无 `-Tail` 需求时用 `[IO.File]::ReadLines(<file>, GetEncoding('GBK'))`。
  - **不用 `-Encoding OEM`**：OEM 代码页区域相关，在英文 Windows 上是 CP437/850，会乱码。
  由于前面已把控制台输出编码设成 UTF-8，进程吐给 Go 的就是 UTF-8。

## 3. 时间范围 —— 字符串比较，而不是正则枚举

ISO 时间戳 `YYYY-MM-DD HH:MM:SS` 是**定长 19 字符**，字典序与时间序完全一致。
所以时间过滤不需要、也不应该用正则枚举起止之间的每一秒（范围一大正则会爆炸），
而是做字符串比较。命令长度恒定，与时间跨度无关。

### Unix：awk，带状态保留续行

```awk
{ if (match($0, /[0-9]{4}-[0-9]{2}-[0-9]{2} [0-9]{2}:[0-9]{2}:[0-9]{2}/)) {
    t=substr($0,RSTART,RLENGTH); keep=(t>=s && t<=e)
  }
  if (keep) print; fflush() }
```

- `-v s=... -v e=...` 传参，避免注入；
- `$script:_keep`（awk 里是普通变量）跨记录保持状态：一行带时间戳，决定它自己以及
  随后**不带时间戳的续行**（如 Java 堆栈）是否保留；
- `fflush()` 保证 follow 模式实时输出。

### Windows：Where-Object，带状态

```powershell
| Where-Object {
    if ($_ -match '<timeTokenPattern>') {
      $t=$Matches[0]
      $script:_keep = ($t -ge '<start>' -and $t -le '<end>')
    }
    $script:_keep
  }
```

`$script:_keep` 在管道对象之间保持状态，同样让无时间戳的堆栈续行跟随上一行的判定。

时间端点由 Go 侧 `TimeBounds` 按粒度对齐成秒级闭区间：

| 粒度   | start 补齐      | end 补齐        |
| ------ | --------------- | --------------- |
| day    | `00:00:00`      | `23:59:59`      |
| hour   | `:00:00`        | `:59:59`        |
| minute | `:00`           | `:59`           |
| second | 原样            | 原样            |

## 4. 内容过滤 —— 一条短正则

级别和内容由 `AssemblePattern` 拼成一条正则交给 `grep -E` / `Select-String`：

- 多个级别用 `|` 做 OR：`(ERROR|WARN)`；
- 级别和内容之间用 `.*` 连接，表示 AND；
- 普通文本模式下内容用 `regexp.QuoteMeta` 转义成字面量；
- 自定义正则（仅在勾选"正则"时生效）优先级最高，直接整条使用，并跳过时间阶段。

**跨平台正则方言注意**：Unix 用的是 `grep -E`（POSIX ERE）和 `awk`，
与 .NET / PCRE 语法有差异，拼装时必须用三家都支持的子集：

- **不能用 `(?:...)` 非捕获组**，那是 PCRE 语法，ERE 不支持，会导致 Linux 下整条正则不匹配。用普通捕获分组 `(...)`（捕获本身无副作用）。
- **awk（尤其 Debian/Ubuntu 默认的 mawk）不支持 `{n}` 区间量词**。时间戳正则在 awk 阶段用显式重复 `[0-9][0-9][0-9][0-9]-...`；Go 的 `regexp` 和 PowerShell 仍可用 `{4}`。
- 用户在"自定义正则"框里输入的内容会原样传给 `grep -E`，需自行保证是 ERE 兼容语法。

### Unix 管道

```
... | grep -E [-i] [-v] [-B N] [-A N] [--line-buffered] '<pattern>'
    | grep -v [-E|-F] [-i] [--line-buffered] '<exclude>'
```

follow 模式下加 `--line-buffered`，避免大块缓冲导致延迟。

### Windows 管道

```
... | Select-String [-CaseSensitive] [-NotMatch] [-Context B,A] -Pattern '<pattern>'
    | Select-String -NotMatch [-SimpleMatch] [-CaseSensitive] -Pattern '<exclude>'
    | ForEach-Object { $_.Line }   # 有上下文时拼成 Pre+Line+Post
```

- 排除关键词独立一轮 `-NotMatch`，对应"排除"语义；
- `-SimpleMatch` 对应普通文本（非正则）模式。

## 5. 原样导出

不经过任何过滤，字节级原样输出：

- Unix：`cat <file>`
- Windows：
  ```powershell
  $c=[IO.File]::OpenRead('<file>')
  try { $c.CopyTo([Console]::OpenStandardOutput()) } finally { $c.Close() }
  ```
  用 .NET 文件流直接拷到标准输出，避免 `Get-Content` 逐行解析带来的开销。

## 6. 引号与转义

所有进入命令的用户可控字符串（文件路径、模式）都必须转义：

- Unix `shQuote`：包单引号，内部 `'` → `'\''`。
- PowerShell `psQuote`：包单引号，内部 `'` → `''`。

时间端点和模式都通过这两个函数注入，杜绝 shell 注入。

## 7. 一条完整示例

跟踪模式、UTF-8、末 200 行、ERROR 或 WARN、含 `timeout`、排除 `health`、
2026-08-13 全天、大小写不敏感：

Unix 大致为：

```sh
tail -F -n 200 '/var/log/app.log' \
  | awk -v s='2026-08-13 00:00:00' -v e='2026-08-13 23:59:59' '<prog>' \
  | grep -E -i --line-buffered '(ERROR|WARN).*timeout' \
  | grep -v -F -i --line-buffered 'health'
```

Windows 为等价的 `Get-Content -Wait` + `Where-Object` + `Select-String` 管道。
