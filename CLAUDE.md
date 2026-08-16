# CLAUDE.md — LogViewer 项目协作规则

## 版本号（重要，AI 不得修改）

- 项目根目录的 `VERSION` 文件是**唯一**的版本号来源。
- 构建脚本 `build_all_platforms.ps1` 与 `go build` 都从该文件读取版本号，
  通过 `-ldflags "-X main.version=$(cat VERSION)"` 注入。
- **版本号只能由开发者（项目唯一开发者）本人手动修改。AI（Claude Code 及任何
  自动化工具）禁止编辑 `VERSION` 文件、禁止自行升版、禁止在提交中改动它。**
- 需要发版时，由开发者改好 `VERSION` 后再让 AI 执行构建/提交。

## 工作原则

- 实事求是、根治问题根源、拒绝糊弄方案、激进优化；禁止治标不治本，杜绝浅层次修补。
- 遇到无法确认的问题不靠主观猜测：通过编写测试用例、新增日志埋点等方式验证，以客观结果为准。
- 架构原则：**一切都是命令**。Go 只是操作系统原生命令的外壳，日志的读取/跟踪/过滤/转码
  全部交给 Unix `tail/cat/grep/awk/iconv` 或 Windows PowerShell 原生命令完成；
  不要为了绕过原生命令的怪癖而在 Go 侧重写其能力。

## 提交前检查

- `go build ./...`、`go vet ./...`、`go test ./...` 通过。
- 改动前端时 `node --check static/app.js` 通过。
- 不要提交 `logviewer.json`、`*.log`、`*.exe`、`dist/`、`.build_tmp/`（已在 .gitignore）。
