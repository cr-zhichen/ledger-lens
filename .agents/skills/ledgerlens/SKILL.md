---
name: ledgerlens
description: "使用 LedgerLens（账镜）CLI 只读查询钱迹账单、账本、资产、分类、标签和预算，管理本地账单缓存，并汇总个人收支。适用于通过本项目查询花销、分析账单、同步缓存或排查 CLI 调用；不用于新增、修改或删除远端账务。"
---

# LedgerLens 调用指南

## 先确定调用入口

在 LedgerLens 仓库根目录执行命令。本 Skill 随仓库放在 `.agents/skills/ledgerlens/`；若被复制到其他位置，先定位实际仓库或已安装的 `ledgerlens`，不要依赖固定的个人目录。

```sh
./bin/ledgerlens --help
./bin/ledgerlens --version
./bin/ledgerlens auth status
```

缺少二进制或需要使其与当前源码一致时，使用仓库工具链构建：

```sh
mise install
mise run build
```

也可使用 `mise run cli -- <命令及参数>`。自动解析输出时优先直接调用二进制，分别读取 stdout、stderr 和退出码。完整参数与错误处理见 [命令参考](references/commands.md)。不要假定存在 `stats`、`export` 或 `--json`；账单统计由调用方完成，正常输出已经是 JSON。

正式版每次调用都会自动检查 GitHub 更新；stderr 中的 `notice.code=UPDATE_AVAILABLE` 是提示，不是失败。命令失败时 `notice` 与 `error` 位于同一个 JSON 对象中，仍按 `error.code` 决策。向用户简要转述发现的更新及下载链接，不自行下载安装。更新请求失败不影响账务命令；`notice.source=cache` 表示本次在线检查失败，应注明这是 `checked_at` 时确认的版本。

用户要求完全离线时添加 `--no-update-check`，或设置 `LEDGERLENS_NO_UPDATE_CHECK=1`。`update check` 是主动联网命令，即使关闭自动检查仍会请求 GitHub，离线任务不要执行它。`version` / `--version` 和 `update check` 均不打开账务数据库；`dev` 构建只支持主动查询，`update_available=null` 表示当前开发版本无法比较。

## 根据会话与任务选择操作

- `auth status` 只检查本地会话及缓存状态，`has_session` 不代表 Token 已通过在线验证。
- 已有会话时直接查询或同步，明确失效的 Token 由 CLI 尝试自动重新登录一次。不要每次查询都重新登录。
- 用户要求离线查询或读取已有缓存时，直接使用 `bills list` / `bills get`，不要求登录，也不添加 `--fresh`。
- 用户询问当前或最近的花销时，先 `sync` 一次，再读取本地分页；首次同步自动全量拉取，之后自动增量同步。单次查询也可使用 `--fresh`。
- 查询指定账本前，用 `books list` 确认 ID。本地账单不指定 `--book` 才表示全部缓存账本；不要用 `--book -1` 代替。
- 只有首次使用或确需重新认证时，使用用户已授权提供的凭据；缺少必需凭据时只询问缺失项。

初始化示例：

```sh
./bin/ledgerlens init --credentials-stdin --non-interactive
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
   ./bin/ledgerlens bills list --since 2026-08-01 --until 2026-09-01 --limit 1000 --offset 0
   ```

3. 从 `data.items` 读取账单，记录 `data.total` 和 `data.synced_at`。将 offset 增加本页实际条数，保留相同筛选条件，直到读完 total。limit 最大为 10000，不能假定一页完整。
4. 若第一页使用 `--fresh`，后续页省略它，避免每页重新同步。发现提前空页、重复 ID、total 或 synced_at 在分页间变化时，重新读取一次完整范围；仍无法确认完整性时，明确说明结果不完整。
5. 保留原始数字精度，再筛选与汇总。常规支出为 `type=0`，收入为 `type=1`；分页 total 包含所有账单类型，不能直接当作消费笔数。不要把收入、转账或其他类型的金额一起加到花销里。
6. 需要分类名称时调用 `categories list`，结合账本与分类 ID 匹配，未知分类保留为“未分类”。最终给出日期范围、支出金额、支出笔数和有用的分类汇总，注明缓存同步时间。

金额、退款、币种、分类与备注解释规则见 [统计口径](references/commands.md#统计口径)。如果用户明确给了时间、账本、离线要求或统计口径，优先采用用户要求。

## 遇到错误时

先读取错误 JSON 中的 `error.code`，不要只看进程退出码。按 [错误处理](references/commands.md#错误处理) 选择下一步。

- `CACHE_EMPTY` 表示尚未初始化缓存，不代表没有花销。
- `INIT_SYNC_FAILED` 表示登录信息已经保存，应重试 `sync`。
- `--fresh` 同步失败时不会返回旧数据；不要将旧缓存的分析描述为已刷新。
- `sync --full` 用于明确需要的全量重建，不作为常规查询前置步骤。同步失败会保留已有缓存。

远端能力限于认证与读取；`init`、`sync`、登录和退出会更新本地状态。完成查询后保留会话和缓存，只有用户要求退出时才调用 `logout`。不要通过清空数据库、切换账号或更换 API 地址来绕过调用错误。
