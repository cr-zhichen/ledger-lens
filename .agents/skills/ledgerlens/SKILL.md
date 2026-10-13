---
name: ledgerlens
description: "使用 LedgerLens（账镜）CLI 查询钱迹账单、账本、资产、分类、标签和预算，新增、修改、删除账单，管理本地缓存并汇总个人收支。适用于查询花销、记账、修正或删除账单、同步缓存和排查 CLI 调用。"
---

# LedgerLens 调用指南

## 先确定调用入口

优先使用已安装并可执行的 `ledgerlens`；从源码使用时，在仓库根目录调用 `./bin/ledgerlens`。本文的 `ledgerlens` 均指选定入口，不依赖固定的个人目录。

```sh
ledgerlens --help
ledgerlens --version
ledgerlens auth status
```

只有源码仓库中缺少二进制或需要与源码一致时，才使用仓库工具链构建：

```sh
mise install
mise run build
```

源码仓库也可使用 `mise run cli -- <命令及参数>`。自动解析输出时优先直接调用二进制，分别读取 stdout、stderr 和退出码。完整参数与错误处理见 [命令参考](references/commands.md)。不要假定存在 `stats`、账单导出或 `--json`；账单统计由调用方完成。

正式版成功检查后 24 小时内复用更新缓存，到期后的调用才会自动请求 GitHub；失败后至少间隔 1 小时再尝试，限流时等待服务器指定的更晚时间。stderr 中的 `notice.code=UPDATE_AVAILABLE` 是提示，不是失败；命令失败时 `notice` 与 `error` 位于同一个 JSON 对象中，仍按 `error.code` 决策。向用户简要转述更新及下载链接，不自行下载安装。`notice.source=cache` 表示使用本地记录，应注明 `checked_at` 是上次成功在线检查时间，不能据此判断本次联网失败。

用户要求完全离线时添加 `--no-update-check`，或设置 `LEDGERLENS_NO_UPDATE_CHECK=1`。`update check` 是主动联网命令，即使关闭自动检查仍会请求 GitHub，离线任务不要执行它。`version` / `--version` 和 `update check` 均不打开账务数据库；`dev` 构建只支持主动查询，`update_available=null` 表示当前开发版本无法比较。

## 获取本版本的 Skill 和提示词

CLI 自带本版本的指南，查看与导出均无需登录、完全离线。命令及文件选项见[Skill 与提示词](references/commands.md#skill-与提示词)。只有可执行文件时，先用 `ledgerlens skill show --file references/commands.md` 读取命令参考。更新 CLI 后按用户要求重新导出；已有文件需要覆盖时，先确认用户的更新授权涵盖这些文件，再使用 `--force`。

`skill show`、`prompt show` 输出纯文本，不能按 JSON 解析；导出命令及账务命令仍输出 JSON。

## 根据会话与任务选择操作

- `auth status` 只检查本地会话及缓存状态，`has_session` 不代表 Token 已通过在线验证。
- 已有会话时直接查询或同步，明确失效的 Token 由 CLI 尝试自动重新登录一次。不要每次查询都重新登录。
- 用户要求离线查询或读取已有缓存时，直接使用 `bills list` / `bills get`，不要求登录，也不添加 `--fresh`。
- 用户询问当前或最近的花销时，先 `sync` 一次，再读取本地分页；首次同步自动全量拉取，之后自动增量同步。单次查询也可使用 `--fresh`。
- 查询指定账本前，用 `books list` 确认 ID。本地账单不指定 `--book` 才表示全部缓存账本；不要用 `--book -1` 代替。
- 只有首次使用或确需重新认证时，使用用户已授权提供的凭据；缺少必需凭据时只询问缺失项。

初始化示例：

```sh
ledgerlens init --credentials-stdin --non-interactive
```

执行工具向进程 stdin 写入一个 JSON 对象，随后**关闭 stdin，发送 EOF**。仅写入换行而保持输入流打开会一直等待。

```json
{"account":"<账号>","password":"<原始密码>"}
```

已有 MD5 时，用 `password_md5` 替换 `password`，不要再次散列。已保存账号时可省略 `account`。不要将凭据写入仓库、文档或普通日志。只需保存会话时改用 `login`；`init` 在登录后还会同步账单。

## 查询并汇总花销

1. 按 `Asia/Shanghai` 解析日期并明确统计口径。默认“近一周”为含今天的最近七个自然日：起始日为今天减六天，结束参数为明天。“上周”则是上周一至本周一。`--since` 包含起始日，`--until` 不包含结束日。
2. 按上一节决定是否同步，再查询日期范围。以下为示例月份，实际调用时替换日期：

   ```sh
   ledgerlens bills list --since 2026-08-01 --until 2026-09-01 --limit 1000 --offset 0
   ```

3. 从 `data.items` 读取账单，记录 `data.total` 和 `data.synced_at`。将 offset 增加本页实际条数，保留相同筛选条件，直到读完 total。limit 最大为 10000，不能假定一页完整。
4. 若第一页使用 `--fresh`，后续页省略它，避免每页重新同步。发现提前空页、重复 ID、total 或 synced_at 在分页间变化时，重新读取一次完整范围；仍无法确认完整性时，明确说明结果不完整。
5. 保留原始数字精度，再筛选与汇总。常规支出为 `type=0`，收入为 `type=1`；分页 total 包含所有账单类型，不能直接当作消费笔数。不要把收入、转账或其他类型的金额一起加到花销里。
6. 需要分类名称时调用 `categories list`，结合账本与分类 ID 匹配，未知分类保留为“未分类”。最终给出日期范围、支出金额、支出笔数和有用的分类汇总，注明缓存同步时间。

金额、退款、币种、分类与备注解释规则见 [统计口径](references/commands.md#统计口径)。如果用户明确给了时间、账本、离线要求或统计口径，优先采用用户要求。

## 新增、修改和删除账单

只执行用户已授权的写入；查询或分析请求不包含写入授权。用户明确给出操作和参数时直接执行，缺少必需信息时只询问缺失项。不要用真实账单测试开发中的功能。

1. 先确认账本、分类、资产和标签；需要 ID 时读取对应目录，不能猜测。修改、删除前定位具体账单，有多个候选时让用户选择。
2. 使用 `bills create --input-stdin`、`bills update --input-stdin` 或 `bills delete --input-stdin`。输入格式、字段、范围和示例统一见[账单写入](references/commands.md#账单写入)。一次输入一个 JSON 对象后关闭 stdin；凭据和账单不写入仓库文件。
3. 修改只传需要变更的字段。CLI 会先同步，再提交和核对结果；不额外清空缓存或切换账号。用户要求离线时不能写入。
4. 删除遇到 `RELATED_BILLS_EXIST` 时，读取源账单和关联账单，明确列出会一起删除的退款、报销记录。只有用户的授权已覆盖这些关联记录时才设置 `cascade_related=true`，否则询问是否全部删除。
5. 成功后用返回的 `data.items` 汇报具体结果与 `synced_at`。失败时检查 `error.details.items`，按[写入错误处理](references/commands.md#错误处理)核对；不能把整个批次重新提交，也不能把确认失败当作远端回滚。

## 遇到错误时

先读取错误 JSON 中的 `error.code`，不要只看进程退出码。按 [错误处理](references/commands.md#错误处理) 选择下一步。

- `CACHE_EMPTY` 表示尚未初始化缓存，不代表没有花销。
- `INIT_SYNC_FAILED` 表示登录信息已经保存，应重试 `sync`。
- `--fresh` 同步失败时不会返回旧数据；不要将旧缓存的分析描述为已刷新。
- `sync --full` 用于明确需要的全量重建，不作为常规查询前置步骤。同步失败会保留已有缓存。

`init`、`sync`、登录和退出会更新本地状态；账单写入命令还会修改远端账单。完成操作后保留会话和缓存，只有用户要求退出时才调用 `logout`。不要通过清空数据库、切换账号或更换 API 地址来绕过调用错误。
