# 账镜 · LedgerLens

账镜是面向 AI Agent 的钱迹 CLI，支持账单管理、转账、退款、报销和收支分析。账单与基础资料缓存在本地，支持离线查询，无需部署服务。

- 读取账单、账本、资产、分类、标签和预算等信息。
- 管理普通收支、转账、信用卡还款、退款和报销。
- 支持原币和跨币种记账，可填写折算金额或启用自动换算。
- 支持高级账单筛选、收支统计和预算使用分析。
- 账务结果输出 JSON，内置 Agent Skill，方便自动调用与汇总。


## 快速开始

从 [GitHub Releases](https://github.com/cr-zhichen/ledger-lens/releases/latest) 下载对应系统和架构的压缩包，解压后运行 `ledgerlens`（Windows 为 `ledgerlens.exe`）。`darwin` 表示 macOS，`amd64` 表示 Intel/AMD x64，`arm64` 表示 ARM64 / Apple Silicon。下载包自带 CLI 和 Agent Skill，无需安装 Go。

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
# 刷新账单与基础资料
./bin/ledgerlens cache refresh

# 查询指定月份的账单，按需替换日期
./bin/ledgerlens bills list --since 2026-09-01 --until 2026-10-01

# 按月统计收支
./bin/ledgerlens bills stats --since 2026-09-01 --until 2026-10-01

# 查看预算使用情况
./bin/ledgerlens budgets get --month 2026-09 --daily

# 查看本地登录与缓存状态
./bin/ledgerlens auth status
```

账单查询默认读取本地缓存，加 `--fresh` 可先同步再查询。日期按北京时间解释，包含起始日、不包含结束日。列表默认每页 100 条，使用 `--limit` 和 `--offset` 分页；汇总前需读完全部结果。

## 账单管理

普通账单使用 `bills`，转账与信用卡还款使用 `transfers`，退款使用 `refunds`，报销使用 `reimbursements`。输入格式、字段及示例见[账单写入](.agents/skills/ledgerlens/references/commands.md#账单写入)。

> 写入命令会直接修改钱迹中的账单，删除无法由本 CLI 撤销。

## 交给 Agent 使用

CLI 内置对应版本的 Skill 和默认提示词，无需登录或联网即可获取：

```sh
./bin/ledgerlens skill show
./bin/ledgerlens prompt show
./bin/ledgerlens skill export --dir ./ledgerlens-skill
./bin/ledgerlens prompt export --output ./ledgerlens-prompt.txt
```

导出目录包含完整的 [LedgerLens Skill](.agents/skills/ledgerlens/SKILL.md)，可交给支持 Skill 的 Agent。更新 CLI 后重新导出即可更新内容；如需覆盖已有文件，添加 `--force`。

支持该 Skill 的 Agent 可以这样调用：

```text
$ledgerlens 查询我近一周的花销，按分类汇总。
$ledgerlens 只使用本地缓存，查询我上个月的收支。
$ledgerlens 记一笔今天的午餐支出 25 元。
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

已发现的新版本通过 stderr 的 JSON `notice` 提示；使用缓存时注明上次检查时间。stdout 与原命令退出码保持原有含义，失败时提示附在同一个错误 JSON 中。网络异常不影响原命令。

账务查询添加 `--offline` 后也会关闭更新检查。其他命令可加 `--no-update-check`，或设置 `LEDGERLENS_NO_UPDATE_CHECK=1`。主动执行 `update check` 仍会联网。更新时下载对应压缩包并替换可执行文件即可，账务数据独立保留。

## 许可证

[MIT](LICENSE)

## 致谢

本项目的钱迹接口实现参考了 [fangzhengjin/qianji-mcp](https://github.com/fangzhengjin/qianji-mcp)，感谢原作者的整理与分享。
