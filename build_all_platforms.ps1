<#
.SYNOPSIS
Go 跨平台一键编译打包脚本 - LogViewer 日志查看工具
#>

# ===================== 配置区（按需修改） =====================
$ProjectName = "logviewer"
$Version = "v1.0.0"
# 程序入口文件，main.go 所在相对路径
$MainPath = "."
# 输出目录
$DistDir = "./dist"
# 编译产物临时目录
$BuildTmpDir = "./.build_tmp"

# 定义目标平台列表 GOOS, GOARCH, 后缀, 压缩格式
$targets = @(
    @{ GOOS="windows"; GOARCH="amd64"; Ext=".exe"; ArchiveType="zip" },
    @{ GOOS="windows"; GOARCH="arm64"; Ext=".exe"; ArchiveType="zip" },
    @{ GOOS="linux";    GOARCH="amd64"; Ext="";     ArchiveType="tar.gz" },
    @{ GOOS="linux";    GOARCH="arm64"; Ext="";     ArchiveType="tar.gz" },
    @{ GOOS="darwin";   GOARCH="amd64"; Ext="";     ArchiveType="tar.gz" },
    @{ GOOS="darwin";   GOARCH="arm64"; Ext="";     ArchiveType="tar.gz" }
)
# ============================================================

Write-Host "===== 开始清理旧构建产物 =====" -ForegroundColor Cyan
if (Test-Path $DistDir) { Remove-Item $DistDir -Recurse -Force }
if (Test-Path $BuildTmpDir) { Remove-Item $BuildTmpDir -Recurse -Force }
New-Item -Path $DistDir,$BuildTmpDir -ItemType Directory -Force | Out-Null

Write-Host "===== 开始跨平台编译 $ProjectName $Version =====" -ForegroundColor Cyan

foreach ($t in $targets) {
    $goos = $t.GOOS
    $goarch = $t.GOARCH
    $ext = $t.Ext
    $archiveType = $t.ArchiveType

    $binName = "${ProjectName}${ext}"
    $tmpOutputDir = Join-Path $BuildTmpDir "${ProjectName}-${Version}-${goos}-${goarch}"
    $binOutputPath = Join-Path $tmpOutputDir $binName

    New-Item -Path $tmpOutputDir -ItemType Directory -Force | Out-Null

    Write-Host "`n[编译] $goos/$goarch -> $binName" -ForegroundColor Green

    # 设置环境变量并执行编译
    $env:GOOS = $goos
    $env:GOARCH = $goarch
    go build -ldflags "-s -w -X main.version=$Version" -o "$binOutputPath" $MainPath

    if (-not (Test-Path $binOutputPath)) {
        Write-Host "❌ $goos/$goarch 编译失败！" -ForegroundColor Red
        continue
    }

    # ========== 打包压缩 ==========
    $archiveName = "${ProjectName}-${Version}-${goos}-${goarch}.${archiveType}"
    $archivePath = Join-Path $DistDir $archiveName

    if ($archiveType -eq "zip") {
        Compress-Archive -Path "$tmpOutputDir/*" -DestinationPath $archivePath -Force
    }
    elseif ($archiveType -eq "tar.gz") {
        # PowerShell 内置没有tar，调用 tar.exe (Windows10+/Git Bash/WSL自带)
        Push-Location $tmpOutputDir
        tar -czf "$archivePath" *
        Pop-Location
    }

    Write-Host "✅ 输出包：$archiveName" -ForegroundColor Green
}

# 清理临时目录
Remove-Item $BuildTmpDir -Recurse -Force
Write-Host "`n===== 全部编译打包完成，产物目录：$DistDir =====" -ForegroundColor Cyan