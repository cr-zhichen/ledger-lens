# 项目 Agent 指南

使用简体中文。

## 调用钱迹 CLI

查询钱迹账单、花销、账本或其他只读资料，或管理账单缓存时，先阅读 [LedgerLens Skill](.agents/skills/ledgerlens/SKILL.md)，按需查阅其中的命令参考。支持 Skill 的 Agent 可通过 `$ledgerlens` 显式调用。

从仓库根目录运行 `./bin/ledgerlens`；缺少二进制时通过 `mise run build` 构建。入口帮助为 `./bin/ledgerlens --help`，本地会话与缓存状态使用 `./bin/ledgerlens auth status`。

当前版本仅调用远端认证与只读账务接口；同步会更新本地缓存。复用已有会话，根据用户要求选择刷新或离线读取。汇总前读完分页，保留金额和 ID 的精度，并说明统计日期与缓存同步时间。凭据和个人账务数据不写入受版本控制的文件。

## 开发约定

- 工具链统一由根目录 `mise.toml` 的 `[tools]` 管理。选择工具版本前先执行 `mise ls --installed <tool>`，优先使用满足约束的已安装版本。
- 项目任务统一定义在 `mise.toml` 的 `[tasks]` 中，通过 `mise run <task>` 执行。
- 根据变更运行必要检查；代码检查入口为 `mise run check`，包含测试、静态检查和构建。不编写仅复述实现的测试，不为 UI 编写单元测试。
- 修改 CLI 命令、输出结构或缓存行为时，同步更新 README 与本项目 Skill，确保示例能由实际 CLI 执行。
