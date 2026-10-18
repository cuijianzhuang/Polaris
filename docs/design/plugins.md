# Polaris 插件系统设计

插件框架已独立建仓：[cuijianzhuang/polaris-plugins](https://github.com/cuijianzhuang/polaris-plugins)。完整设计、协议和独立插件示例以新仓库为准，本页保留 Polaris 宿主侧的接入索引。

- [完整架构与协议设计](https://github.com/cuijianzhuang/polaris-plugins/blob/main/docs/design.md)
- [仓库边界与实施路线](https://github.com/cuijianzhuang/polaris-plugins/blob/main/docs/roadmap.md)
- [外部通知插件清单示例](https://github.com/cuijianzhuang/polaris-plugins/blob/main/examples/external-notify/polaris-plugin.json)
- [插件框架贡献指南](https://github.com/cuijianzhuang/polaris-plugins/blob/main/CONTRIBUTING.md)

[返回项目首页](../../README.md)

## 当前状态

已实现：API 令牌与权限范围、续期 / 加流量接口、结构化事件与 Webhook、插件管理器（安装握手、配置、健康检查、后台页面、用户中心入口、卸载）。开发文档、SDK 和可运行示例在 polaris-plugins 仓库，与本实现的差异见其 `docs/compatibility.md`。

尚未实现：远程订阅渲染 / 改写、节点外部指标写入（计划中的第 4 阶段）。

## 仓库分工

| 仓库 | 负责内容 |
| --- | --- |
| **polaris-plugins** | 版本化插件协议、清单、公共校验与签名工具、语言 SDK、独立服务示例、插件开发文档 |
| **Polaris** | API scope、订单事务、宿主数据库迁移、结构化事件与 outbox、Webhook 调度、安装配置与生命周期、后台与用户中心入口、订阅适配 |
| **polaris-license** | 产品授权服务；插件不能绕过专业版授权或取得授权签名私钥 |

外部插件通过公开 HTTP API 与宿主通信，不导入 Polaris 的 `internal` 包，也不访问宿主数据库。可复用工具在新仓库开发，宿主鉴权、持久化和后台任务生命周期仍由 Polaris 管理。

## 宿主实施阶段

1. **API 基础**：细分权限、按令牌过滤 OpenAPI、事务订单幂等与并发更新。
2. **结构化事件与 Webhook**：事件数据、事务 outbox、签名、租约调度、重试与审计。
3. **安装与页面**：配对、加密配置、健康检查、权限批准、动态扩展菜单、iframe 与普通用户入口。
4. **订阅与指标**：远程格式、受限改写、隔离缓存、独立指标写入权限和配套文档。

四个阶段指 Polaris 宿主 PR；新仓库以对应的独立 PR 提供协议、工具与示例。双方 PR 互相引用并明确兼容版本，不能因新仓库已有清单就宣称宿主已经支持安装。

## 菜单与分支约定

插件入口使用侧边栏一级菜单 **「扩展」**，与「用户」「订阅」「系统」同级，位于「域名与证书」之后、「系统」之前。菜单与宿主 UI 适配继续放在 Polaris。宿主入口为 `/extensions`，当前提供已有 API 令牌管理；移动端完整菜单与快捷搜索使用同一入口，节点管理员不显示扩展管理。

所有开发使用独立开发分支和 PR，不直接推送 `main`；新插件仓库初始化分支为 `develop`。宿主 PR 需验证社区版和专业版；前端执行翻译检查、TypeScript 检查及构建。插件仓库按所引入工具和示例执行对应的协议、签名、兼容与 HTTP 行为验证。
