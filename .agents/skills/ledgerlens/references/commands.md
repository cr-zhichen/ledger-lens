# CLI 命令参考

## 调用与返回格式

下表省略 `ledgerlens` 前缀；源码仓库中可使用 `./bin/ledgerlens`。以当前二进制 `--help` 为准；参数不符时先核对帮助。

```text
ledgerlens [--db PATH] [--timeout 30s] <命令> [参数]
```

`--db`、`--timeout` 和 `--no-update-check` 可放在整个命令前或叶子命令的参数中，例如 `ledgerlens --db PATH bills list` 或 `ledgerlens bills list --db PATH`，不要放在 `bills` 与 `list` 之间。`--timeout` 是单个 HTTP 请求的超时，默认 30 秒，完整同步可能持续更久；自动更新检查独立限制为 2 秒。

- 成功：退出码 0；除 `skill show`、`prompt show` 的纯文本外，stdout 为 `{"ok":true,"data":...}`。
- 失败：非零退出码，stderr 为 `{"ok":false,"error":{"code":"...","message":"..."}}`。
- 帮助信息：纯文本。使用 `mise run cli` 时，运行器可能额外输出诊断信息。
- 更新提示：成功或帮助命令的 stderr 为 `{"notice":{"code":"UPDATE_AVAILABLE",...}}`；失败时将 `notice` 附在原错误对象上，stderr 仍是单个 JSON 对象。只有 notice 不表示失败，仍以退出码及 `error.code` 决策。

普通查询复用默认数据库或用户指定的 `LEDGERLENS_DB` / `--db`。默认位置是系统用户配置目录下的 `ledger-lens/ledgerlens.sqlite`，macOS 为 `~/Library/Application Support/ledger-lens/ledgerlens.sqlite`。

## 版本与更新

| 命令 | 行为与输出 |
| --- | --- |
| `version` / `--version` | 返回 `data.version`、`commit`、`build_date`；不打开账务数据库。 |
| `update check` | 主动请求 GitHub 最新公开正式版，不受缓存期限和自动退避间隔限制，遵循 `--timeout`；返回 `current_version`、`latest_version`、`update_available`、`release_url`、`checked_at`、`source`。不登录、不读取账务数据库。 |
| `--no-update-check` | 关闭本次自动检查及提示；也可设置 `LEDGERLENS_NO_UPDATE_CHECK=1`。不影响显式执行 `update check`。 |

除 `skill`、`prompt` 命令外，正式版每次调用（含帮助、版本及错误命令）都会读取更新状态；成功检查后 24 小时内直接复用缓存，到期后的下一次调用才并行请求 GitHub。自动请求最多 2 秒，失败后至少间隔 1 小时再尝试；遇到 `Retry-After` 或额度耗尽的 `X-RateLimit-Reset` 时，以服务器指定的更晚时间为准。自动检查由调用触发，不启动定时后台服务。

已有新版本时每次继续提示，不缓存“已读”状态；升级后重新比较当前版本，旧提示自动消失。`source=cache` 只表示复用本地记录，`checked_at` 始终是上次成功在线确认时间，不表示本次联网失败。主动检查始终请求远端，成功会重新计算 24 小时有效期，失败会报错并更新自动退避时间，不将缓存冒充在线结果。

共享缓存目录的并发调用只允许一个到期自动检查执行 HTTP，其他调用立即使用已有缓存。请求开始前会记录重试间隔，进程中断也不会让下一次调用立刻重试；文件锁随进程退出自动释放。缓存无法写入时跳过自动联网，账务命令正常执行，手动 `update check` 仍可联网。

比较按主版本、次版本、补丁版本的数值顺序执行，例如 `1.10.0 > 1.9.0`。仅接受 `vX.Y.Z` 正式版，拒绝草稿及预发布。`dev` 不自动检查；主动检查可获取最新 Release，但 `update_available=null`，不能据此声称当前开发源码落后。正式版 `update_available=false` 表示远端版本不高于当前版本，不会建议降级。

更新检查不发送钱迹账号、Token 或账务内容。缓存仅保存版本 Tag、成功检查时间与最早重试时间，独立位于系统用户缓存目录的 `ledger-lens/update.json`（macOS 为 `~/Library/Caches/ledger-lens/update.json`），相邻的 `update.json.lock` 用于进程互斥。兼容旧版只含 Tag 和检查时间的缓存。更新功能只检查和提示，不自动下载、替换或重启程序。

## Skill 与提示词

以下命令只使用当前 CLI 自带的内容，无需登录，不联网、不检查更新，也不打开账务数据库。

| 命令 | 参数与输出 |
| --- | --- |
| `skill show` | 输出 Skill Markdown。`--file` 默认 `SKILL.md`，也可指定 `references/commands.md` 或 `agents/openai.yaml`。 |
| `skill export --dir PATH` | 导出完整 Skill 目录；PATH 中直接包含 `SKILL.md`、`references/commands.md`、`agents/openai.yaml`。 |
| `prompt show` | 输出默认提示词文本，可直接复制给 Agent。 |
| `prompt export --output PATH` | 将默认提示词保存为 UTF-8 文本文件。 |

两种导出命令默认不覆盖文件；`--force`（默认 false）允许覆盖对应文件，Skill 目录中的其他文件会保留。成功 JSON 的 `data` 包含 `version`、`commit`、`build_date` 和导出文件的绝对路径列表 `files`。更新 CLI 后重新导出即可获取新版本内容，已导出的文件不会自动更新。

## 认证与缓存

| 命令 | 作用与参数 |
| --- | --- |
| `auth status` | 本地状态：`data.has_session`、`auto_login`、`uid`、`cache.initialized`、`cache.synced_at`。不发出在线验证请求。 |
| `init --credentials-stdin --non-interactive` | 登录保存会话，再同步账单；已有缓存时执行增量同步。 |
| `login --credentials-stdin` | 仅登录并保存会话。 |
| `sync` | 首次全量同步，之后增量同步全部账本的账单。 |
| `sync --full` | 完整拉取成功后重建当前账号的账单缓存。 |
| `cache refresh` | 刷新账单、用户、账本、正常与隐藏账户、分类、标签和币种目录。预算、成员和借贷详情在对应查询时缓存。 |
| `logout` | 清除本地密码 MD5 和 Token，保留账号、用户 ID、设备标识和账单。 |

凭据 stdin 接收一个 JSON 对象后需关闭输入流。`account` 可在已有账号时省略，`password` 与 `password_md5` 二选一。不要将 `--credentials-stdin` 与 `--account`、`--password`、`--password-md5` 混用；不支持 stdin 的执行工具可使用后者的参数形式，但不要回显含凭据的完整命令。

`--non-interactive` 为兼容参数，所有命令本身都不弹出问答。`init` / `login` 支持 `--api-url`；一般沿用默认的 `https://api.qianjiapp.com` 或已有配置，不为普通查询改地址。

数据库以明文保存账号、密码 MD5、Token、设备标识和账单，不保存原始密码；macOS / Linux 文件权限为 `0600`，Windows 使用所在用户目录的 ACL。通过 `auth status` 检查状态，不要为普通查询导出数据库或读取凭据表。缓存按 API 地址和用户 ID 隔离；用户 ID 是不透明字符串。

同步将完整分页中的新增、更新、删除与最终同步点一起提交。全量与增量同步失败均保留旧快照；单次同步最多 200 页、约 256 MiB 数据。

## 本地账单

| 命令 | 参数与返回 |
| --- | --- |
| `bills list` | `--book ID`、`--since YYYY-MM-DD`、`--until YYYY-MM-DD`、`--currency USD`、`--limit 100`、`--offset 0`、`--fresh` 或 `--offline`。limit 为 1～10000，offset 非负。省略 book 查询全部缓存账本。 |
| `bills list --filter-stdin` | 从 stdin 读取下表的 JSON 筛选条件，可加 `--fresh` 或 `--offline`，不能同时使用其他筛选参数。 |
| `bills get --id ID` | 可加 `--fresh` 或 `--offline`；`data` 直接是原始账单对象。 |

列表的 `data` 包含 `items`、`total`、`limit`、`offset`、`source: "cache"`、`synced_at` 和 `initialized`。total 是筛选范围内所有类型的账单总数。空 items 且 total 为 0 才表示已初始化缓存中的该范围没有账单。

日期按北京时间解释；since 包含当天零点，until 不包含当天零点。账单默认只查本地；添加 `--fresh` 时必须先同步成功。

`--filter-stdin` 的字段如下。省略字段表示不限制，`since/until` 和创建时间使用 Unix 秒，结束边界均不包含。

| 字段 | 含义与默认值 |
| --- | --- |
| `book_id`、`since`、`until` | 账本及发生时间；默认全部缓存账本、全部时间。 |
| `created_since`、`created_until` | 创建时间范围。 |
| `types` | 类型数字数组，例如 `[0,1,5]`。 |
| `min_money`、`max_money` | 原币主金额范围，包含边界。 |
| `category_ids`、`include_subcategories` | 分类 ID 数组；默认包含子分类，需要分类缓存。 |
| `tag_ids`、`tag_match` | 标签 ID 数组；`any`（默认）或 `all`。 |
| `remark_keyword` | 备注子串，不区分大小写。 |
| `asset_ids` | 匹配主账户、转出或转入账户中的任意一个。 |
| `asset_id`、`from_asset_id`、`target_asset_id` | 分别精确匹配主账户、转出和转入账户。 |
| `member_ids` | 记账人 ID 字符串数组。 |
| `source` | 0 手动、2 导入、120 周期、121 分期、122 自动记账。 |
| `currency` | 原始记账币种；必要时读取账户和用户缓存。 |
| `no_asset`、`no_tags` | 只查无账户或无标签的记录，默认 false。 |
| `exclude_from_income_expense`、`exclude_from_budget` | 按统计开关筛选；省略表示不限，false 表示未开启。 |
| `sort` | `time_desc`（默认）、`time_asc`、`money_desc` 或 `money_asc`。 |
| `limit`、`offset` | 默认 100、0。 |

```json
{"types":[0,5],"tag_ids":["tag-food"],"remark_keyword":"午餐","min_money":20,"exclude_from_income_expense":false,"sort":"money_desc","limit":100}
```

## 统计与预算

```sh
ledgerlens bills stats --since 2026-09-01 --until 2026-10-01 --fresh
ledgerlens bills stats --currency USD --unit month --offline
ledgerlens bills stats --filter-stdin --unit day
ledgerlens budgets get --book -1 --month 2026-09 --daily --fresh
ledgerlens budgets get --book -1 --year 2026 --offline
```

`bills stats` 支持账单列表的日期、账本、币种参数或同一套 `--filter-stdin`，统计全部匹配记录，忽略分页。`--unit` 默认 day，也可为 month、year。返回 `summary`、`counts`、`categories`、`members`、`time_series`、`average`、`currency`、`synced_at`；日均分母只计算已结束的完整日历日。

收支默认按本位币统计；指定 `--currency USD` 则只统计原币为 USD 的记录。退款、报销通过源账单关联处理，子账单不重复计入收支；转账和信用卡还款本金单列，手续费计入支出。两个统计开关分别作用于收支与预算。缺少历史折算金额会报错，不能把未折算外币直接加到本位币中。

`budgets get` 的 `--book` 默认 -1，`--month` 或 `--year` 必须二选一。返回预算总额、已用、剩余、分类使用情况；`--daily` 返回逐日支出、累计支出和剩余预算，默认 false。月份遵循账本配置的月份起始日。`--offline` 仅读取缓存，缺少对应期间的预算时先联网查询一次；`--fresh` 刷新账单和基础资料后再分析。

## 账单写入

以下写入命令均需要登录并联网，通过 `--input-stdin` 接收一个 JSON 对象，输入完成后关闭 stdin。单笔操作也使用只有一项的列表。

| 命令 | 输入 |
| --- | --- |
| `bills create --input-stdin` | `{"bills":[{"type":0,"money":18.50,"category_id":"123","remark":"午餐"}]}` |
| `bills update --input-stdin` | `{"updates":[{"id":"456","patch":{"money":20,"remark":"午餐含饮料"}}]}` |
| `bills delete --input-stdin` | `{"deletions":[{"id":"456"}]}` |

示例中的分类与账单 ID 必须替换为查询得到的真实 ID。调用命令后粘贴 JSON 并发送 EOF；自动调用时由执行工具将 JSON 写入 stdin 后关闭输入流。

新增时 `type` 必填：0 为支出，1 为收入。以下字段用于新增账单或修改的 `patch`；除下述折算金额规则外，修改时省略的字段保持不变。`patch` 不能为空，不能修改收支类型。

| 字段 | 用途 | 新增时默认值 |
| --- | --- | --- |
| `money` | 原币主金额，数字，必须大于 0、最多两位小数，上限 9999999999.99 | 必填 |
| `currency` | 原始记账币种，例如 USD | 账户币种；未关联账户时为本位币 |
| `target_amount` | 优惠后到账或扣款金额，使用账户币种；跨币种时填写或自动换算 | 同币种时使用实际金额 |
| `auto_convert` | 查询汇率补齐所需折算金额，布尔值；显式金额优先 | false |
| `base_amount` | 外币账单优惠后实际金额对应的本位币折算金额，数字，0～9999999999.99，最多两位小数 | 省略，仅记原币 |
| `category_id` | 对应账本和收支类型的分类 ID，字符串 | 必填 |
| `book_id` | 账本 ID，字符串 | `"-1"`，默认账本 |
| `asset_id` | 资产 ID，字符串；`"-1"` 表示不关联资产 | `"-1"` |
| `time` | 记账时间，Unix 秒时间戳；自然语言日期按 Asia/Shanghai 转换 | 当前时间 |
| `remark` | 备注；传 `""` 可清空 | 空字符串 |
| `discount` | 支出优惠，数字，最多两位小数，不超过 money；实际支出为 money 减 discount | 0 |
| `tag_ids` | 标签 ID 字符串列表；传 `[]` 可清空 | 空列表 |
| `reimbursable` | 支出是否待报销，布尔值 | false |
| `exclude_from_income_expense` | 不计入收支，布尔值 | false |
| `exclude_from_budget` | 不计入预算，布尔值 | false |

两个统计开关独立设置，可同时开启。修改时省略某个开关就保留原值，传 `false` 取消该开关。

选择 USD 账户时，`money:10` 表示 10 USD，新增和修改均按账户原币处理；未选择账户时使用本位币。原币与账户币种不同时，填写 `currency` 和 `target_amount`，或设置 `auto_convert:true`。

已知本位币折算金额时可传 `base_amount`，本位币币种由 CLI 读取钱迹配置；本位币账户不接受此参数。可以单独修改 `base_amount`。开启自动换算后，CLI 只补齐缺少的金额；已传金额优先。`bs/bv` 始终同时保存或省略。

原币金额或优惠发生变化时，需要重新提供跨币种到账金额或开启自动换算；没有新本位币金额且未开启自动换算时移除旧折算值。只改备注、分类、标签或统计开关时保留历史金额。修改 `money` 使用原币，不使用账户折算后的金额。

修改已有优惠账单的 `money` 时保留原优惠；传 `discount:0` 可取消优惠。已有退款或报销关联的源账单，仅支持修改备注、时间、分类、标签和两个统计开关。

以下示例中的资产 ID 需替换为查询得到的 USD 账户：

```json
{"bills":[{"type":0,"money":10,"category_id":"123","asset_id":"456","exclude_from_income_expense":true,"exclude_from_budget":true}]}
```

修改原币金额、填写已知的本位币折算金额，并恢复计入预算，保留另一个开关：

```json
{"updates":[{"id":"789","patch":{"money":20,"base_amount":142.50,"exclude_from_budget":false}}]}
```

删除支持普通收支、待报销支出、转账和信用卡还款。每项填写 `id`；只有确定连同关联退款、报销记录一起删除时，才添加 `"cascade_related":true`（默认 false）。退款和报销子账单使用下述独立命令；借贷、分期等特殊账单写入请在钱迹中完成。

### 转账与信用卡还款

`transfers create --input-stdin` 创建单笔，`transfers update --input-stdin` 接收 `{"id":"...","patch":{...}}`；删除使用 `bills delete`。

```json
{"money":100,"from_asset_id":"10","target_asset_id":"20","credit_repayment":true,"fee":1,"auto_convert":true}
```

转出、转入账户和 `money` 必填；金额使用转出账户币种。`credit_repayment` 默认 false，true 时转入账户必须为信用卡。`fee`、`discount` 默认 0，不能同时大于 0，也不能超过主金额。修改手续费会取消旧优惠，修改优惠会取消旧手续费。

| 同币种示例 | 实际转出 | 实际到账 |
| --- | --- | --- |
| `money:100, fee:10` | 100 | 90 |
| `money:100, discount:10` | 90 | 100 |

跨币种的 `target_amount`、`base_amount` 填写上述到账金额的折算值；自动换算也使用到账金额。

支持 `book_id`、`time`、`remark`、`tag_ids`、两个统计开关、`target_amount`、`base_amount` 和 `auto_convert`，含义见上表。不填写普通账单的 `category_id`、`asset_id`、`reimbursable` 或 `currency`。转出、转入账户不能相同，借贷账户不支持转账。

### 退款

| 命令 | 输入示例 |
| --- | --- |
| `refunds create --input-stdin` | `{"source_id":"100","money":20,"asset_id":"10","remark":"退货"}` |
| `refunds update --input-stdin` | `{"id":"101","patch":{"money":15,"remark":"部分退款"}}` |
| `refunds delete --input-stdin` | `{"id":"101"}` |

退款使用源账单原币。新增必须填写 `source_id`、`money`，源账单必须是普通支出或待报销支出。`asset_id` 默认沿用原账单，传 `"-1"` 不关联账户；可传 `time`、`remark`、`tag_ids` 及三个换算参数 `target_amount`、`base_amount`、`auto_convert`。时间默认当前时间且晚于源账单。修改省略字段保留原值，不能更换源账单。独立删除只删除退款记录并更新原关联。

### 报销

```sh
ledgerlens reimbursements create --input-stdin
ledgerlens reimbursements delete --input-stdin
ledgerlens reimbursements cancel --input-stdin
```

```json
{"source_ids":["100","101"],"money":150,"asset_id":"10","remark":"差旅报销","auto_convert":true}
```

新增必须填写待报销源账单 ID 列表 `source_ids` 和总金额 `money`，同一批使用相同原币。可传 `asset_id`（默认不关联账户）、`time`（默认当前时间）、`remark`、`tag_ids` 和三个换算参数。总金额按源账单时间从早到晚分配，最后一笔接收余量；可以超过待报销余额。确认总额与源账单范围后再调用。

删除一条报销入账传 `{"id":"201"}`；取消一批源账单的全部报销关系传 `{"source_ids":["100","101"]}`，会删除这些源账单关联的报销入账记录。报销只是标记待报销状态时，仍使用 `bills update` 的 `reimbursable`。

`confirm_upgrade` 默认 false。只有钱迹返回 `REIMBURSEMENT_UPGRADE_REQUIRED` 且用户明确接受“升级后旧版本将不能继续使用报销”，才以相同业务参数加 `"confirm_upgrade":true` 重试；升级只在服务端要求时执行。

成功返回 `data.items` 与 `data.synced_at`。每项包含 `index`（结果列表中从 0 开始的序号）、`id`（字符串）、`status`、`confirmed`；新增和修改还返回完整 `bill`。关联删除的源账单与子账单分别列出。只有所有项目写入并同步核对成功才返回退出码 0。

发生写入错误时，逐项结果位于 `error.details.items`：`applied` 表示钱迹已接受，`conflict` 表示发生冲突，`unknown` 表示尚无法确认。`confirmed:true` 表示已在同步结果中核对；没有 `synced_at` 表示本次写后同步未成功。先按错误处理表处理，不能根据非零退出码推断远端未改动。

## 标签写入

| 命令 | 输入示例 |
| --- | --- |
| `tags create --input-stdin` | `{"name":"出差","group_id":"g1","color":"#529af8"}` |
| `tags archive --input-stdin` | `{"id":"2610206ad73282cd1f51344"}` |

新建只需 `name`，首尾空白会被去除，不超过 50 个字符。`group_id` 为 `tags list` 返回的标签组 ID，省略时放入默认组；`color` 为 `#RRGGBB`，省略时由钱迹决定。任何状态（含已归档）下已有同名标签时返回 `TAG_EXISTS`，`error.details.id` 为已有标签 ID，直接使用它，不再新建。

归档只接受当前正常状态的标签，已归档或不存在时返回 `TAG_NOT_ACTIVE`；本 CLI 不提供取消归档。归档成功后以 `tags list --status 2` 的结果确认。

成功返回 `data.id`、`status`（`applied`）、`confirmed`（已在标签列表核对）、`tag` 与 `synced_at`，并刷新 `tags list` 缓存。失败时 `error.details` 含 `id` 和 `status`；`WRITE_UNCERTAIN` 与 `WRITE_CONFIRMATION_FAILED` 先用 `tags list` 按名称或 ID 核对，不直接重试。

## 资产与预算写入

| 命令 | 输入示例 |
| --- | --- |
| `assets set-balance --input-stdin` | `{"asset_id":"1792508970392","balance":-20.50,"record_diff":true,"category_id":"171588388","remark":"对账"}` |
| `budgets set --input-stdin` | `{"book_id":"-1","month":"2026-10","total":3000,"categories":[{"category_id":"101","money":500},{"category_id":"102","money":0}]}` |

`assets set-balance` 把账户余额校准为 `balance`（可为负数，最多两位小数）。`asset_id` 来自 `assets list`，包括隐藏账户。余额与当前相同时返回 `INVALID_ARGUMENT`；借贷账户和有分期的信用卡返回 `ASSET_UNSUPPORTED`，请在钱迹中处理。默认只改余额、不记账；`record_diff:true` 时钱迹在默认账本按当前时间记一笔差额账单，余额减少记为支出，增加记为收入。只有记差额账单时才能填写以下字段：
- `category_id`：须与差额方向一致，减少用支出分类，增加用收入分类，可用 `categories list --type 0|1` 查询。省略时由钱迹选择“其它”分类。
- `remark`：备注。
- `exclude_from_income_expense`、`exclude_from_budget`：统计开关。

成功返回 `asset_id`、`previous_balance`、`balance`、`status`、`confirmed`（已重新读取账户核对余额）和完整 `asset`；记差额账单时另含 `diff_bill_id` 与写后同步的 `synced_at`。

`budgets set` 设置一个账本某月（`month`）或某年（`year`）的预算，两者必须且只能选一项；`book_id` 默认 `-1`。
- `total` 为总预算，必须大于 0。
- `categories` 只列出要变更的支出分类预算：`money` 为新额度，`0` 表示删除该分类预算；未列出的保持不变。

CLI 先联网读取当期全部预算，合并变更后整体提交，这与钱迹 App 的提交方式一致。总预算不能删除，也不能小于一级分类预算之和（已设预算的父分类下的子分类不重复计入）。仅调整分类预算时，总预算会像 App 一样自动提高到这个和。不能删除当期最后一项预算。

成功返回期间、`confirmed`（已按钱迹返回的当期预算核对）、`total`、全部分类预算 `categories` 及 `synced_at`，并更新 `budgets list` 缓存。预算功能需要钱迹会员，非会员时读取和设置均返回 `VIP_REQUIRED`，不会写入。

两个命令失败时，`error.details` 含已知的部分结果。`WRITE_UNCERTAIN` 与 `WRITE_CONFIRMATION_FAILED` 先用 `assets list` 或 `budgets list` 核对，不直接重试。

## 基础资料

| 命令 | 参数 |
| --- | --- |
| `user info` | 用户资料、VIP 状态及期限、本位币和多币种状态。 |
| `books list` | `--include-hidden` 包含隐藏账本，`--name` 按名称子串筛选。 |
| `books members` | `--book ID`，默认 `-1`。 |
| `assets list` | `--status 0\|2`，默认 0；0 正常，2 隐藏。`--name` 按名称子串筛选。 |
| `assets debts` | 必须指定 `--direction 51\|52`，`--status 0\|1` 默认 0。方向值的业务含义须有依据再解释。 |
| `categories list` | `--book ID` 默认 `-1`；`--type -1\|0\|1` 默认 -1，分别为全部、支出、收入分类。 |
| `tags list` | `--status -1\|1\|2` 默认 -1，1 正常，2 已归档，按每个标签的 `status` 在本地筛选；`--lasttime 0`。 |
| `budgets list` | `--book ID` 默认 `-1`；`--month YYYY-MM` 与 `--year YYYY` 必须且只能选一项。需要钱迹会员。 |
| `currencies list` | 无专属参数。 |
| `assets get` | 必须指定 `--id ID`，返回单个资产账户详情。 |
| `assets groups` | 资产分组，无专属参数。 |
| `assets trend` | 资产余额快照（`assetid`、`time` 秒、`value`）。钱迹一次返回全部快照，CLI 在本地筛选：`--id ID` 只看一个账户，`--month YYYY-MM` 或 `--year YYYY` 按北京时间筛选期间，均可省略；筛选期间时另返回 `since`、`until`。 |
| `installments list` | 分期列表，无专属参数；自动读完全部分页。 |
| `recurring list` | 周期记账任务，无专属参数；自动读完全部分页。 |
| `savings list` | 存钱计划，无专属参数。 |

这些命令默认联网读取并缓存，可加 `--offline` 完全离线查询；返回 `source` 和 `synced_at`。`cache refresh` 不刷新资产详情、资产分组、资产走势、分期、周期记账和存钱计划，这些资料需先联网查询一次才能离线读取；钱迹对部分功能有会员限制，读取失败时报告错误码，不推测为没有数据。分页合并后的结果位于 `data.list`，`data.pages` 为读取的页数。上述资料的字段以实际返回为准，含义不明的字段原样报告，不推测金额或期数含义。目录内容保留远端数据结构，读取实际返回字段。账本、成员、分类等列表通常位于 `data.list`；资产可能分组，标签为包含 `tags` 的分组，不要把分组数当作标签数。分类有 `parentid`、`level` 等层级字段。远端 book 默认值 `-1` 是协议哨兵值，不能等同于本地查询的“全部账本”。

## 直接拉取变更页

优先使用 `sync`。仅在确需读取原始远端分页或排查协议时使用：

```sh
./bin/ledgerlens bills pull --book -1 --pageoffset 0
./bin/ledgerlens bills pull --cursor-stdin
```

参数还有 `--pagesign SIGN`、`--lasttimes JSON`。`--cursor-stdin` 与这些游标参数及 `--book` 互斥；输入一个对象后关闭 stdin，例如：

```json
{"bookid":"-1","pageoffset":0,"pagesign":""}
```

返回 `data.changes`、`deletes`、`categories`、`has_more`、`next_cursor`、`lasttimes`，不会写入缓存。它是一页变更，不能当作完整的当前账单集合。

`has_more=true` 时，原样传入 `next_cursor` 读取下一页，保留本轮初始 lasttimes；bookid 也可能在分页中变化，不要固定为首页值。完整一轮结束后的下一轮增量读取，从全局 `bookid=-1`、`pageoffset=0`、空 pagesign 开始，使用上一轮最终 lasttimes。

## 统计口径

- JSON 金额可能为数字或字符串，账单等 ID 可能超出 JavaScript 安全整数范围。使用无损 JSON 解析和精确十进制，例如 Python `json.loads(text, parse_float=Decimal)` 再以 `Decimal` 处理金额字符串。不要先经 JavaScript `Number` 或二进制浮点丢失精度后再转换。ID 保留为字符串或任意精度整数，用户 ID 不转换为数字。
- 优先使用上述内置统计；自行汇总时先确定用户要普通消费还是完整收支口径，不能把列表 total 当作消费笔数。
- `extra.flag` 缺省为 0；`flag & 1` 非零表示不计入收支，`flag & 2` 非零表示不计入预算。一般收支汇总排除前者，预算统计排除后者；明细查询按用户要求决定是否保留。
- 检查负数、退款、报销、关联账单与特殊类型，按有依据的业务含义处理，不推测类型码、不重复抵扣。无法确认的项目单列并说明口径。
- 历史统计只使用保存的原币或折算金额，不按当前汇率重算。`extra.curr` 的 `ss/sv` 为原币及主金额，`ts/tv` 为账户币种及实际金额，`bs/bv` 为本位币及实际金额的折算值。普通收支 `money` 是账户金额；退款、报销和转账的金额含义按各命令解释，不能统一累加原始 `money`。
- 结合 `bookid` 与 `cateid` 匹配分类，保留层级及收支类型，避免把不同账本或收入与支出的同名分类混在一起。缺失分类不能丢弃账单。
- `remark` 可能为空，`descinfo` / `fromact` 可能描述资金账户。账户名不能当作商户名或商品名称；缺少信息时如实说明。
- 输出只包含完成用户问题所需的汇总或明细。标明日期、统计口径与同步时间，完整读取前不声称结果完整；旧缓存不描述为实时数据。

## 错误处理

| error.code | 下一步 |
| --- | --- |
| `INVALID_ARGUMENT` | 核对 `--help`，修正日期、ID、必需参数或互斥参数。 |
| `EXPORT_EXISTS` | 目标文件已存在；确认要更新这些文件后添加 `--force`，或换一个导出位置。 |
| `EXPORT_TARGET_INVALID`、`EXPORT_FAILED` | 检查导出位置、目录权限和剩余空间，或更换路径。 |
| `BUNDLE_INVALID` | CLI 的内置内容无法读取，重新安装对应版本。 |
| `REFERENCE_NOT_FOUND`、`BOOK_EXPIRED` | 重新查询对应目录或在钱迹中处理账本状态，再调整输入。 |
| `WRITE_BUSY` | 当前写入尚未开始，等待另一个写入命令结束后重试。 |
| `BILL_NOT_OWNED` | 只能修改或删除当前账号创建的账单；核对目标，不切换账号绕过限制。 |
| `BILL_UNSUPPORTED` | 当前操作不支持该账单，请在钱迹中完成。 |
| `CURRENCY_AMOUNT_MISSING` | 缺少历史折算金额，按原币统计或根据用户提供的信息补充；不自动改账单。 |
| `ASSET_CURRENCY_MISSING` | 账户币种资料缺失；刷新账户资料后再查询，不按本位币猜测金额。 |
| `CURRENCY_PRICE_UNAVAILABLE` | 当前币种缺少有效汇率，改为显式填写折算金额。 |
| `RELATED_BILL_MISSING` | 关联账单缓存缺失，执行全量同步后再统计。 |
| `REIMBURSEMENT_UPGRADE_REQUIRED` | 解释升级影响，取得用户明确同意后才设置 confirm_upgrade。 |
| `REIMBURSEMENT_UPGRADED_BUT_FAILED` | 升级已完成但报销未完成；先同步核对，不重复升级。 |
| `RELATED_BILLS_EXIST` | 根据消息核对退款、报销关联；关联删除必须已获得覆盖全部记录的授权。 |
| `WRITE_PARTIAL`、`WRITE_CONFLICT` | 阅读 `error.details.items`，保留成功项目，不重试整个批次；先同步核对其余项目。 |
| `WRITE_UNCERTAIN` | 请求可能已经生效，先执行 `sync`（标签写入执行 `tags list`，余额校准执行 `assets list`，预算执行 `budgets list`），按返回的 ID 和原参数逐项核对；不能自动重试新增、修改、删除、归档、校准或预算提交。 |
| `WRITE_CONFIRMATION_FAILED` | 钱迹已接受写入，本地确认未完成。只重试对应的读取命令（`sync`、`tags list`、`assets list` 或 `budgets list`）并核对结果，不再次写入。 |
| `ASSET_UNSUPPORTED` | 借贷账户或有分期的信用卡不能用本 CLI 校准余额，请用户在钱迹中操作。 |
| `VIP_REQUIRED` | 该功能需要钱迹会员，未写入任何数据；告知用户，不重试。 |
| `TAG_EXISTS` | 已有同名标签，使用 `error.details.id`，不另建同名标签。 |
| `TAG_NOT_ACTIVE` | 标签不存在或已归档；用 `tags list` 核对，不重复归档。 |
| `LIST_LIMIT` | 分页超过 50 页限制，报告结果不完整，不当作完整列表汇总。 |
| `UPDATE_NOT_FOUND` | 没有可读取的公开正式版 Release，可能尚未发布或仓库不可访问；不能据此声称已是最新版。 |
| `UPDATE_RATE_LIMITED`、`UPDATE_CHECK_FAILED` | 主动更新检查失败，稍后重试，不重新登录钱迹。自动检查遇到这些错误不影响原命令。 |
| `UPDATE_INVALID` | GitHub 返回的版本不是有效正式版，报告错误，不建议安装。 |
| `AUTH_REQUIRED` | 在线任务需要登录；复用已授权凭据，缺失时询问。离线账单读取不需重新登录。 |
| `CACHE_EMPTY` | 缓存未初始化；任务允许联网时同步，无会话则初始化。用户只要离线结果时说明尚无缓存。 |
| `INIT_SYNC_FAILED` | 会话已保存，重试 `sync`，并按消息中的原始错误码排查，不重复提交密码。 |
| `TOKEN_EXPIRED` | 读取和写前同步可自动重登一次；写入提交不会自动重登重发。确需认证时使用已授权凭据登录，随后同步核对再决定后续操作。 |
| `LOGIN_REJECTED` | 登录被拒绝，停止重复使用同一凭据，请用户确认所需登录信息。 |
| `ACCOUNT_MISMATCH` | 用户 ID 不一致，停止操作并说明账号冲突，不覆盖或混用缓存。 |
| `STATE_CHANGED` | 本地状态被并发更新；重新读取状态，同一已授权账号下可重试一次。 |
| `HTTP_ERROR` | 临时网络问题可重试一次；不因此重新登录。刷新未成功时明确说明，只有任务允许时才用注明同步时间的旧缓存。 |
| `SIGNATURE_REJECTED`、`BUSINESS_ERROR`、`RESPONSE_INVALID` | 报告返回码，检查协议或上游兼容性，不重复登录或改 API 地址试错。 |
| `SYNC_CURSOR_INVALID`、`SYNC_INVALID`、`SYNC_LIMIT`、`RESPONSE_TOO_LARGE` | 同步未完成；保留缓存，报告具体错误，检查分页、数据格式或容量限制。 |
| `NOT_FOUND` | 当前缓存中找不到该 ID，不代表远端一定不存在；核对 ID，按任务需要刷新后再查。 |
| `DATABASE_ERROR`、`INTERNAL_ERROR` | 检查路径、权限和本地执行环境，不清空数据库。 |
| `CANCELED` | 停止调用，不自动重启被取消的工作。 |

退出码分类：1 本地存储或状态错误；2 参数错误；3 认证错误；4 远端请求或同步错误；5 缓存未就绪或账单不存在；130 取消。优先根据具体 error.code 决策。
