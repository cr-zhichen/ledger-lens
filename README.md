# 账镜 · LedgerLens

面向 AI Agent 的钱迹只读 CLI。把账单缓存在本地，方便查询花销、分析收支，无需部署服务。

- 读取账单、账本、资产、分类、标签和预算等信息。
- 首次全量同步，后续增量更新，支持离线查询。
- 输出 JSON，内置 Agent Skill，方便自动调用与汇总。

当前版本仅支持远端登录与查询，收支统计由 Agent 或调用方完成。

## 快速开始

从 [GitHub Releases](https://github.com/cr-zhichen/ledger-lens/releases/latest) 下载对应系统和架构的压缩包，解压后运行 `ledgerlens`（Windows 为 `ledgerlens.exe`）。`darwin` 表示 macOS，`amd64` 表示 Intel/AMD x64，`arm64` 表示 ARM64 / Apple Silicon。下载包自带 CLI 和 Agent Skill，无需安装 Go。

也可准备好 `mise`，在项目根目录从源码构建：

```sh
mise install
mise run build
```

首次使用，登录并同步账单：

```sh
./bin/ledgerlens init --credentials-stdin --non-interactive
```

通过标准输入传入登录信息，写入后关闭输入流（EOF）。在 macOS / Linux 终端中，粘贴 JSON、回车，再按 `Ctrl-D`：

```json
{"account":"<账号>","password":"<原始密码>"}
```

也可用 `password_md5` 替换 `password`，传入已有的密码 MD5。登录信息会保存在本地，后续查询无需重复初始化。

## 常用命令

```sh
# 更新本地账单缓存
./bin/ledgerlens sync

# 查询指定月份的账单，按需替换日期
./bin/ledgerlens bills list --since 2026-09-01 --until 2026-10-01

# 查看账本
./bin/ledgerlens books list

# 查看本地登录与缓存状态
./bin/ledgerlens auth status
```

账单查询默认读取本地缓存，加 `--fresh` 可先同步再查询。日期按北京时间解释，包含起始日、不包含结束日。列表默认每页 100 条，使用 `--limit` 和 `--offset` 分页；汇总前需读完全部结果。

## 交给 Agent 使用

项目内置 [LedgerLens Skill](.agents/skills/ledgerlens/SKILL.md)，支持该 Skill 的 Agent 可以这样调用：

```text
$ledgerlens 查询我近一周的花销，按分类汇总。
$ledgerlens 只使用本地缓存，查询我上个月的收支。
```

其他 Agent 可从 [AGENTS.md](AGENTS.md) 了解调用方式。完整命令、参数和常见问题见 [使用参考](.agents/skills/ledgerlens/references/commands.md)。

## 本地数据

账单、账号、密码 MD5 和 Token 保存在本地 SQLite 中，不保存原始密码，内容未额外加密。执行 `./bin/ledgerlens logout` 可清除登录凭据，账单缓存仍可离线查询。

## 版本与更新

```sh
./bin/ledgerlens --version
./bin/ledgerlens update check
```

正式版成功检查后 **24 小时内复用本地缓存**；到期后的下一次调用才会并行请求 GitHub，最多等待 2 秒。检查失败后至少间隔 **1 小时**再尝试，限流时遵循服务器指定的更晚时间。并发调用共享检查状态，避免重复联网。

已发现的新版本仍会在每次调用时通过 stderr 的 JSON `notice` 提示，直到当前程序升级；使用缓存时注明上次检查时间。stdout 与原命令退出码保持原有含义，失败时提示附在同一个错误 JSON 中。网络异常不影响原命令。

完全离线时加 `--no-update-check`，或设置 `LEDGERLENS_NO_UPDATE_CHECK=1`。主动执行 `update check` 不受缓存期限和退避间隔限制，仍会联网，失败返回错误。更新检查只提供版本和下载链接，安装时下载对应压缩包并替换可执行文件即可，账务数据独立保留。开发构建显示 `dev`，不自动比较版本，可主动查询最新发布。

## 构建与发布

正式版以 Tag 为版本来源：`v1.2.3` 自动注入程序版本 `1.2.3`，无需修改版本文件。这里只接受 `vX.Y.Z` 正式版，不接受预发布后缀或数字前导零。

将代码提交并推送到 GitHub 后，创建并推送正式版 Tag：

```sh
git tag -a v0.1.0 -m "Release v0.1.0"
git push origin v0.1.0
```

GitHub Actions 会先在 macOS、Linux、Windows 上执行检查，再构建三个系统各自的 amd64 / arm64 压缩包，生成 `SHA256SUMS` 并发布正式版 Release。普通 push 到 main 和 PR 会自动运行 CI，不保存临时 Actions 构建产物。

本地使用 `mise run check` 检查、`mise run package` 试打包；`RELEASE_TAG=v0.1.0 mise run release` 可复现正式版构建，要求 Tag 已存在、指向 HEAD 且工作区干净。日常 `mise run build` 仅在干净的正式版 Tag 上使用对应版本，其余标记为 `dev`。

## 许可证

[MIT](LICENSE)

## 致谢

本项目的钱迹接口实现参考了 [fangzhengjin/qianji-mcp](https://github.com/fangzhengjin/qianji-mcp)，感谢原作者的整理与分享。
