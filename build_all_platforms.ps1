<#
.SYNOPSIS
Go 跨平台一键编译打包脚本 - LogViewer 日志查看工具
#>

# ===================== 配置区（按需修改） =====================
# 脚本最顶部【配置区下方】添加
$oldGOOS = $env:GOOS
$oldGOARCH = $env:GOARCH
$ProjectName = "logviewer"
# 版本号唯一来源是项目根目录的 VERSION 文件，只由开发者本人手动修改。
$Version = (Get-Content -Path "$PSScriptRoot/VERSION" -Raw).Trim()
if (-not $Version) { throw "无法从 VERSION 文件读取版本号" }
# main.go 在项目根目录
$MainPath = "."
# 输出目录
$DistDir = "./dist"
# 编译产物临时目录
$BuildTmpDir = "./.build_tmp"

# 定义目标平台列表 GOOS, GOARCH, 后缀, 压缩格式
$targets = @(
    @{ GOOS = "windows"; GOARCH = "amd64"; Ext = ".exe"; ArchiveType = "zip" },
    @{ GOOS = "windows"; GOARCH = "arm64"; Ext = ".exe"; ArchiveType = "zip" },
    @{ GOOS = "linux"; GOARCH = "amd64"; Ext = ""; ArchiveType = "tar.gz" },
    @{ GOOS = "linux"; GOARCH = "arm64"; Ext = ""; ArchiveType = "tar.gz" },
    @{ GOOS = "darwin"; GOARCH = "amd64"; Ext = ""; ArchiveType = "tar.gz" },
    @{ GOOS = "darwin"; GOARCH = "arm64"; Ext = ""; ArchiveType = "tar.gz" }
)
# ============================================================

Write-Host "===== 开始清理旧构建产物 =====" -ForegroundColor Cyan
try {
    # 优先杀掉正在运行的logviewer进程，释放文件锁
    Get-Process -Name $ProjectName -ErrorAction SilentlyContinue | Stop-Process -Force
    Start-Sleep -Milliseconds 200

    # 容错删除目录
    if (Test-Path $DistDir) {
        try {
            Remove-Item $DistDir -Recurse -Force -ErrorAction Stop
        }
        catch {
            Write-Warning "⚠️ dist目录无法完整删除(文件占用)，尝试清空内部文件继续"
            Get-ChildItem $DistDir -Force -ErrorAction SilentlyContinue | ForEach-Object {
                try {
                    $_ | Remove-Item -Recurse -Force -ErrorAction Stop
                }
                catch {
                    Write-Warning "跳过锁定文件：$($_.FullName)"
                }
            }
        }
    }
    if (Test-Path $BuildTmpDir) {
        try {
            Remove-Item $BuildTmpDir -Recurse -Force -ErrorAction Stop
        }
        catch {
            Write-Warning "⚠️ .build_tmp 删除失败，忽略继续构建"
        }
    }

    New-Item -Path $DistDir, $BuildTmpDir -ItemType Directory -Force | Out-Null

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

        # 设置交叉编译环境变量
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
            # 修复tar跨目录打开失败问题：先在临时目录打包，再移动
            $localTar = "${ProjectName}-${Version}-${goos}-${goarch}.tar.gz"
            Push-Location $tmpOutputDir
            tar -czf $localTar *
            Pop-Location
            Move-Item -Path (Join-Path $tmpOutputDir $localTar) -Destination $archivePath -Force
        }

        Write-Host "✅ 输出包：$archiveName" -ForegroundColor Green
    }

    # 清理临时目录，允许失败不报错
    Remove-Item $BuildTmpDir -Recurse -Force -ErrorAction SilentlyContinue
    Write-Host "`n===== 全部编译打包完成，产物目录：$DistDir =====" -ForegroundColor Cyan
}
finally {
    $env:GOOS = $oldGOOS
    $env:GOARCH = $oldGOARCH
    Write-Host "`n环境变量已还原：GOOS=$env:GOOS GOARCH=$env:GOARCH" -ForegroundColor Gray
}