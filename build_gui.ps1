<#
.SYNOPSIS
构建 LogViewer 桌面客户端（GUI）二进制 —— 仅 Windows。

.DESCRIPTION
使用 -tags gui,production 把 Wails v2 窗口壳编进来，产物为双击即可打开窗口的单 exe。
- Wails v2 运行时强制要求 production/dev/bindings 之一的 build tag，否则启动弹错
  "Wails applications will not build without the correct build tags"。
- Wails v2 在 Windows 是纯 Go + WebView2（无 CGO 依赖），因此可在本机直接
  交叉编译 amd64/arm64。-H windowsgui 去掉控制台黑窗。

Web-only 构建（浏览器访问，跨 6 平台）请用 build_all_platforms.ps1。
GUI 模式日志写入 %AppData%\LogViewer\logviewer-gui.log。
#>
# 脚本最顶部【配置区下方】添加
$oldGOOS = $env:GOOS
$oldGOARCH = $env:GOARCH
$ErrorActionPreference = "Stop"

$ProjectName = "logviewer"
# 版本号唯一来源是项目根目录的 VERSION 文件，只由开发者本人手动修改。
$Version = (Get-Content -Path "$PSScriptRoot/VERSION" -Raw).Trim()
if (-not $Version) { throw "无法从 VERSION 文件读取版本号" }

$MainPath = "."
$DistDir = "./dist"
try {
  New-Item -Path $DistDir -ItemType Directory -Force | Out-Null

  Write-Host "===== 开始构建 GUI 客户端 $ProjectName $Version（Windows） =====" -ForegroundColor Cyan

  # Wails v2 Windows 端零 CGO，CGO_ENABLED=0 可交叉编译两个架构。
  $env:CGO_ENABLED = "0"
  $env:GOOS = "windows"

  foreach ($arch in "amd64", "arm64") {
    $env:GOARCH = $arch
    $outName = "$ProjectName-gui-$Version-windows-$arch.exe"
    $outPath = Join-Path $DistDir $outName

    Write-Host "`n[编译] windows/$arch -> $outName" -ForegroundColor Green
    # -H windowsgui：生成窗口子系统程序，不弹控制台。
    # -tags gui,production：gui 编入 Wails 壳；production 是 Wails v2 运行时必需的标记。
    go build -tags "gui,production" `
      -ldflags "-s -w -H windowsgui -X main.version=$Version" `
      -o "$outPath" $MainPath

    if (-not (Test-Path $outPath)) {
      Write-Host "❌ windows/$arch 编译失败！" -ForegroundColor Red
      exit 1
    }
    Write-Host "✅ 输出：$outName" -ForegroundColor Green
  }

  Write-Host "`n===== GUI 构建完成，产物目录：$DistDir =====" -ForegroundColor Cyan
  Write-Host "提示：Win10 早期版本若提示缺 WebView2 Runtime，请安装微软 Edge WebView2 Runtime（Win11 自带）。" -ForegroundColor Yellow
}
finally {
  $env:GOOS = $oldGOOS
  $env:GOARCH = $oldGOARCH
  Write-Host "`n环境变量已还原：GOOS=$env:GOOS GOARCH=$env:GOARCH" -ForegroundColor Gray
}