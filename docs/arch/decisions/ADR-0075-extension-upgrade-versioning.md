# ADR-0075: Extension Upgrade Versioning

## 状态
Accepted（已执行）

## 背景
现有系统在扩展更新和版本管理机制上缺失。原 `extension_catalog` 和 `extension_instances` 没有记录扩展版本，不支持在保留原状态和 ID 的情况下安全更新版本。扩展的卸载逻辑采用级联硬删，会清理现有数据和上下文，并不适合升级场景（GD-13-003）。

## 决策
在 Layer 0 (目录层) 和 Layer 1 (安装层) 增加版本管理，同时增加独立的安全增量升级端点，避免使用级联硬删：

1. **Schema 变更**：
   - `extension_catalog` 表新增 `version` 列，用于缓存从市场同步的最新版本。
   - `extension_instances` 表新增 `installed_version` 列，记录实际安装运行的版本。

2. **新增升级端点**：
   - 添加 `POST /v1/plugins/{id}/upgrade`（实现于 `plugin/manage.go`，并在 `server_routes.go` 注册），该端点负责增量升级，保留现有 `extension_instances.id` 以及 `install_path`，防止触发卸载过程中的副作用和硬删除。

3. **范围声明**：
   - 这是对 ADR-0019 未覆盖的"升级生命周期"的补充。

## 后果
- 扩展可以基于版本号检测是否有更新。
- 用户可以安全更新扩展版本而不丢失状态。
