---
name: ledgerlens
description: "使用 LedgerLens（账镜）CLI 管理钱迹账单、转账、信用卡还款、退款、报销和标签，处理跨币种金额，筛选账单、统计收支、分析预算，查询分期、周期记账、存钱计划与资产走势，及管理离线缓存。适用于查询花销、记账、退款报销、转账还款、预算分析和资产负债查询。"
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

源码仓库也可使用 `mise run cli -- <命令及参数>`。自动解析输出时优先直接调用二进制，分别读取 stdout、stderr 和退出码。完整参数与错误处理见 [命令参考](references/commands.md)。收支统计使用 `bills stats`，预算分析使用 `budgets get`；高级筛选和跨币种写入参数以命令参考为准，不假定存在独立的 `stats`、账单导出或 `--json`。

正式版成功检查后 24 小时内复用更新缓存，到期后的调用才会自动请求 GitHub；失败后至少间隔 1 小时再尝试，限流时等待服务器指定的更晚时间。stderr 中的 `notice.code=UPDATE_AVAILABLE` 是提示，不是失败；命令失败时 `notice` 与 `error` 位于同一个 JSON 对象中，仍按 `error.code` 决策。向用户简要转述更新及下载链接，不自行下载安装。`notice.source=cache` 表示使用本地记录，应注明 `checked_at` 是上次成功在线检查时间，不能据此判断本次联网失败。

用户要求完全离线时添加 `--no-update-check`，或设置 `LEDGERLENS_NO_UPDATE_CHECK=1`。`update check` 是主动联网命令，即使关闭自动检查仍会请求 GitHub，离线任务不要执行它。`version` / `--version` 和 `update check` 均不打开账务数据库；`dev` 构建只支持主动查询，`update_available=null` 表示当前开发版本无法比较。

## 获取本版本的 Skill 和提示词

CLI 自带本版本的指南，查看与导出均无需登录、完全离线。命令及文件选项见[Skill 与提示词](references/commands.md#skill-与提示词)。只有可执行文件时，先用 `ledgerlens skill show --file references/commands.md` 读取命令参考。更新 CLI 后按用户要求重新导出；已有文件需要覆盖时，先确认用户的更新授权涵盖这些文件，再使用 `--force`。

`skill show`、`prompt show` 输出纯文本，不能按 JSON 解析；导出命令及账务命令仍输出 JSON。

## 根据会话与任务选择操作

- `auth status` 只检查本地会话及缓存状态，`has_session` 不代表 Token 已通过在线验证。
- 已有会话时直接查询或同步，明确失效的 Token 由 CLI 尝试自动重新登录一次。不要每次查询都重新登录。
- 用户要求离线时添加 `--offline`，它也关闭自动更新检查；不要求登录，不添加 `--fresh`。账单、统计和基础资料都可读取缓存，资料缺失时报告缺失，不自行联网补齐。
- 用户询问当前或最近的花销时，明细先 `sync` 一次；收支或预算分析先 `cache refresh`，取得账单、用户和分类等资料。单次查询也可使用 `--fresh`。
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

## 查询、统计和预算

1. 按 `Asia/Shanghai` 解释日期，`--since` 包含起始日，`--until` 不包含结束日。先按用户要求确定是否刷新。
2. 查询明细用 `bills list`，复杂条件用 `--filter-stdin`，可按类型、金额、分类、标签、账户、成员、备注、币种和统计开关筛选。完整筛选字段见[本地账单](references/commands.md#本地账单)。
3. 收支汇总优先用 `bills stats`；预算使用情况用 `budgets get`。结果基于全部匹配账单，不受分页 limit 影响。具体口径见[统计与预算](references/commands.md#统计与预算)。
4. 本位币统计需要账单保存的折算金额；缺少时按原币分别统计或报告缺失，不能将外币金额直接当成本位币。统计不会用当前汇率重算历史账单。
5. 只有需要完整明细或自行实现用户指定口径时才逐页读取 `data.items`，直到读完 `total`。后续页不要重复 `--fresh`，保留精度并核对 `synced_at` 一致。发现分页变化时重新读取一次；仍不完整就说明限制。
6. 汇报日期范围、币种、收支或预算结果及缓存同步时间；不把缓存描述成实时数据。分类及账户名从目录读取，不能从账户名推测商户。
7. 分期、周期记账、存钱计划、资产详情、分组和走势使用 `installments list`、`recurring list`、`savings list`、`assets get`、`assets groups`、`assets trend`，参数见[基础资料](references/commands.md#基础资料)。它们不随 `cache refresh` 刷新；字段按实际返回解释，含义无依据时原样报告。

## 新增、修改和删除账单

只执行用户已授权的写入；查询或分析请求不包含写入授权。用户明确给出操作和参数时直接执行，缺少必需信息时只询问缺失项。不要用真实账单测试开发中的功能。

1. 先确认账本、分类、账户及其币种和标签；需要 ID 时读取对应目录，不能猜测。修改、删除前定位具体账单，有多个候选时让用户选择。
2. 普通账单使用 `bills`，转账及信用卡还款使用 `transfers`，退款使用 `refunds`，报销使用 `reimbursements`。命令、字段及示例统一见[账单写入](references/commands.md#账单写入)。每次向 `--input-stdin` 写入一个 JSON 对象后关闭 stdin；凭据和账单不写入仓库文件。
3. 修改只传需要变更的字段。CLI 会先同步，再提交和核对结果；不额外清空缓存或切换账号。用户要求离线时不能写入。
4. 删除遇到 `RELATED_BILLS_EXIST` 时，读取源账单和关联账单，明确列出会一起删除的退款、报销记录。只有用户的授权已覆盖这些关联记录时才设置 `cascade_related=true`，否则询问是否全部删除。
5. 金额优先使用用户明确提供的原币、到账和本位币金额；用户需要自动换算时设置 `auto_convert:true`，不要自行猜测汇率。只修改备注等信息时保留历史折算值。
6. 钱迹要求升级报销时，先说明“升级后旧版本将不能继续使用报销”；只有用户明确同意后，才用 `confirm_upgrade:true` 重试。普通报销授权不包含此次升级。
7. 成功后用返回的 `data.items` 汇报具体结果与 `synced_at`。失败时检查 `error.details.items`，按[写入错误处理](references/commands.md#错误处理)核对；不能把整个批次重新提交，也不能把确认失败当作远端回滚。
8. 记账需要的标签不存在时，只有用户授权新建标签后才执行 `tags create`；遇到 `TAG_EXISTS` 使用返回的已有 ID。归档标签使用 `tags archive`。字段见[标签写入](references/commands.md#标签写入)。
9. 用户授权后，用 `assets set-balance` 校准账户余额；只有用户要求记录差额时才设置 `record_diff:true`。用 `budgets set` 设置月度或年度预算，`categories` 只列要变更的分类，`money:0` 表示删除。字段与限制见[资产与预算写入](references/commands.md#资产与预算写入)。预算需要钱迹会员，遇到 `VIP_REQUIRED` 时告知用户，不重试。

## 遇到错误时

先读取错误 JSON 中的 `error.code`，不要只看进程退出码。按 [错误处理](references/commands.md#错误处理) 选择下一步。

- `CACHE_EMPTY` 表示尚未初始化缓存，不代表没有花销。
- `INIT_SYNC_FAILED` 表示登录信息已经保存，应重试 `sync`。
- `--fresh` 同步失败时不会返回旧数据；不要将旧缓存的分析描述为已刷新。
- `sync --full` 用于明确需要的全量重建，不作为常规查询前置步骤。同步失败会保留已有缓存。

`init`、`sync`、登录和退出会更新本地状态；账单、标签、资产余额和预算写入命令还会修改远端数据。完成操作后保留会话和缓存，只有用户要求退出时才调用 `logout`。不要通过清空数据库、切换账号或更换 API 地址来绕过调用错误。
