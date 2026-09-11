# CLI 命令参考

## 调用与返回格式

下表省略 `./bin/ledgerlens` 前缀。以当前二进制 `--help` 为准；参数不符时先核对帮助与源码。

```text
ledgerlens [--db PATH] [--timeout 30s] <命令> [参数]
```

`--db`、`--timeout` 和 `--no-update-check` 可放在整个命令前或叶子命令的参数中，例如 `ledgerlens --db PATH bills list` 或 `ledgerlens bills list --db PATH`，不要放在 `bills` 与 `list` 之间。`--timeout` 是单个 HTTP 请求的超时，默认 30 秒，完整同步可能持续更久；自动更新检查独立限制为 2 秒。

- 成功：退出码 0，stdout 为 `{"ok":true,"data":...}`。
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

正式版每次调用（含帮助、版本及错误命令）都会读取更新状态；成功检查后 24 小时内直接复用缓存，到期后的下一次调用才并行请求 GitHub。自动请求最多 2 秒，失败后至少间隔 1 小时再尝试；遇到 `Retry-After` 或额度耗尽的 `X-RateLimit-Reset` 时，以服务器指定的更晚时间为准。自动检查由调用触发，不启动定时后台服务。

已有新版本时每次继续提示，不缓存“已读”状态；升级后重新比较当前版本，旧提示自动消失。`source=cache` 只表示复用本地记录，`checked_at` 始终是上次成功在线确认时间，不表示本次联网失败。主动检查始终请求远端，成功会重新计算 24 小时有效期，失败会报错并更新自动退避时间，不将缓存冒充在线结果。

共享缓存目录的并发调用只允许一个到期自动检查执行 HTTP，其他调用立即使用已有缓存。请求开始前会记录重试间隔，进程中断也不会让下一次调用立刻重试；文件锁随进程退出自动释放。缓存无法写入时跳过自动联网，账务命令正常执行，手动 `update check` 仍可联网。

比较按主版本、次版本、补丁版本的数值顺序执行，例如 `1.10.0 > 1.9.0`。仅接受 `vX.Y.Z` 正式版，拒绝草稿及预发布。`dev` 不自动检查；主动检查可获取最新 Release，但 `update_available=null`，不能据此声称当前开发源码落后。正式版 `update_available=false` 表示远端版本不高于当前版本，不会建议降级。

更新检查不发送钱迹账号、Token 或账务内容。缓存仅保存版本 Tag、成功检查时间与最早重试时间，独立位于系统用户缓存目录的 `ledger-lens/update.json`（macOS 为 `~/Library/Caches/ledger-lens/update.json`），相邻的 `update.json.lock` 用于进程互斥。兼容旧版只含 Tag 和检查时间的缓存。更新功能只检查和提示，不自动下载、替换或重启程序。

## 认证与缓存

| 命令 | 作用与参数 |
| --- | --- |
| `auth status` | 本地状态：`data.has_session`、`auto_login`、`uid`、`cache.initialized`、`cache.synced_at`。不发出在线验证请求。 |
| `init --credentials-stdin --non-interactive` | 登录保存会话，再同步账单；已有缓存时执行增量同步。 |
| `login --credentials-stdin` | 仅登录并保存会话。 |
| `sync` | 首次全量同步，之后增量同步全部账本的账单。 |
| `sync --full` | 完整拉取成功后重建当前账号的账单缓存。 |
| `logout` | 清除本地密码 MD5 和 Token，保留账号、用户 ID、设备标识和账单。 |

凭据 stdin 接收一个 JSON 对象后需关闭输入流。`account` 可在已有账号时省略，`password` 与 `password_md5` 二选一。不要将 `--credentials-stdin` 与 `--account`、`--password`、`--password-md5` 混用；不支持 stdin 的执行工具可使用后者的参数形式，但不要回显含凭据的完整命令。

`--non-interactive` 为兼容参数，所有命令本身都不弹出问答。`init` / `login` 支持 `--api-url`；一般沿用默认的 `https://api.qianjiapp.com` 或已有配置，不为普通查询改地址。

数据库以明文保存账号、密码 MD5、Token、设备标识和账单，不保存原始密码；macOS / Linux 文件权限为 `0600`，Windows 使用所在用户目录的 ACL。通过 `auth status` 检查状态，不要为普通查询导出数据库或读取凭据表。缓存按 API 地址和用户 ID 隔离；用户 ID 是不透明字符串。

同步将完整分页中的新增、更新、删除与最终同步点一起提交。全量与增量同步失败均保留旧快照；单次同步最多 200 页、约 256 MiB 数据。

## 本地账单

| 命令 | 参数与返回 |
| --- | --- |
| `bills list` | `--book ID`、`--since YYYY-MM-DD`、`--until YYYY-MM-DD`、`--limit 100`、`--offset 0`、`--fresh`。limit 为 1～10000，offset 非负。省略 book 查询全部缓存账本。 |
| `bills get --id ID` | 可加 `--fresh`；`data` 直接是原始账单对象。 |

列表的 `data` 包含 `items`、`total`、`limit`、`offset`、`source: "cache"`、`synced_at` 和 `initialized`。total 是筛选范围内所有类型的账单总数。空 items 且 total 为 0 才表示已初始化缓存中的该范围没有账单。

日期按北京时间解释；since 包含当天零点，until 不包含当天零点。账单默认只查本地；添加 `--fresh` 时必须先同步成功。

## 远端只读资料

| 命令 | 参数 |
| --- | --- |
| `books list` | `--include-hidden` 包含隐藏账本。 |
| `books members` | `--book ID`，默认 `-1`。 |
| `assets list` | `--status 0\|2`，默认 0；0 正常，2 隐藏。 |
| `assets debts` | 必须指定 `--direction 51\|52`，`--status 0\|1` 默认 0。方向值的业务含义须有依据再解释。 |
| `categories list` | `--book ID` 默认 `-1`；`--type -1\|0\|1` 默认 -1，分别为全部、支出、收入分类。 |
| `tags list` | `--status -1\|1\|2` 默认 -1；`--lasttime 0`。 |
| `budgets list` | `--book ID` 默认 `-1`；`--month YYYY-MM` 与 `--year YYYY` 必须且只能选一项。 |
| `currencies list` | 无专属参数。 |

这些接口保留远端数据结构，读取实际返回字段。账本、成员、分类等列表通常位于 `data.list`；资产可能分组，标签为包含 `tags` 的分组，不要把分组数当作标签数。分类有 `parentid`、`level` 等层级字段。远端 book 默认值 `-1` 是协议哨兵值，不能等同于本地查询的“全部账本”。

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
- 常规花销只汇总 `type=0` 的 `money`；收入 `type=1` 单独统计。消费笔数是筛选后的数量，不是分页 total。只有用户询问净收支时才考虑收入与支出的差额。
- 检查负数、退款、报销、关联账单与特殊类型，按有依据的业务含义处理，不推测类型码、不重复抵扣。无法确认的项目单列并说明口径。
- 多币种分别汇总。仅在币种和换算依据明确时换算，不假定所有账单为人民币，也不为省事截断到两位小数；保留精度至最终展示。
- 结合 `bookid` 与 `cateid` 匹配分类，保留层级及收支类型，避免把不同账本或收入与支出的同名分类混在一起。缺失分类不能丢弃账单。
- `remark` 可能为空，`descinfo` / `fromact` 可能描述资金账户。账户名不能当作商户名或商品名称；缺少信息时如实说明。
- 输出只包含完成用户问题所需的汇总或明细。标明日期、统计口径与同步时间，完整读取前不声称结果完整；旧缓存不描述为实时数据。

## 错误处理

| error.code | 下一步 |
| --- | --- |
| `INVALID_ARGUMENT` | 核对 `--help`，修正日期、ID、必需参数或互斥参数。 |
| `UPDATE_NOT_FOUND` | 没有可读取的公开正式版 Release，可能尚未发布或仓库不可访问；不能据此声称已是最新版。 |
| `UPDATE_RATE_LIMITED`、`UPDATE_CHECK_FAILED` | 主动更新检查失败，稍后重试，不重新登录钱迹。自动检查遇到这些错误不影响原命令。 |
| `UPDATE_INVALID` | GitHub 返回的版本不是有效正式版，报告错误，不建议安装。 |
| `AUTH_REQUIRED` | 在线任务需要登录；复用已授权凭据，缺失时询问。离线账单读取不需重新登录。 |
| `CACHE_EMPTY` | 缓存未初始化；任务允许联网时同步，无会话则初始化。用户只要离线结果时说明尚无缓存。 |
| `INIT_SYNC_FAILED` | 会话已保存，重试 `sync`，并按消息中的原始错误码排查，不重复提交密码。 |
| `TOKEN_EXPIRED` | CLI 已尝试可用的自动重登流程。确需认证时使用已授权凭据登录一次；缺少或被拒绝时询问，不盲目循环。 |
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
