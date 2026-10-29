# Polaris 文档索引

[返回项目首页](../README.md)

按读者分为四类：用户手册（管理员和订阅用户都看这里）、运维专题、对接与插件、设计说明。不确定从哪里开始时，先读[用户使用手册](user-guide.md)。

## 用户使用手册

[user-guide.md](user-guide.md) 覆盖从安装到日常维护的全部操作，常用章节：

| 想做的事 | 章节 |
| --- | --- |
| 第一次部署主控、接入节点 | [安装与部署](user-guide.md#installation)、[首次配置](user-guide.md#first-connection) |
| 拿到订阅后连接 | [客户端使用](user-guide.md#client-use)、[用户中心](user-guide.md#user-center) |
| 管理节点、内核、探针、回程检测 | [节点与内核管理](user-guide.md#nodes) |
| 入站、模板、证书、Snell | [入站、模板与证书](user-guide.md#inbounds) |
| 用户、套餐、商店、优惠券、端口转发商品 | [用户、授权与套餐](user-guide.md#users) |
| 订阅设置、订阅转换、处理组 | [订阅设置与订阅转换](user-guide.md#subscriptions) |
| CDN、CDN 优选 IP、中转、路由、WARP | [域名、CDN、中转与路由](user-guide.md#networking)、[CDN 配置与排查](user-guide.md#cdn-setup) |
| Telegram、邮件、登录、通行密钥、通知策略、API 令牌、Webhook | [Telegram、登录与审计](user-guide.md#security) |
| 升级（含小批升级）、备份、迁移、高可用 | [升级、备份与迁移](user-guide.md#maintenance) |
| 专业版与许可证 | [专业版与许可证](user-guide.md#license) |
| 遇到问题 | [故障排查](user-guide.md#troubleshooting)、[兼容性与功能边界](user-guide.md#compatibility) |
| 环境变量与命令 | [配置与命令参考](user-guide.md#configuration) |

## 运维专题

| 文档 | 内容 |
| --- | --- |
| [后台账号与角色](operations/admin-roles.md) | 超级管理员、节点运维、只读运维、客服、财务只读的权限范围 |
| [付款恢复、退款和运行健康](operations/recovery-and-health.md) | 在线订单的补单、退款流程和运行健康检查 |

## 对接与插件

| 文档 | 内容 |
| --- | --- |
| [API 令牌与订单接口](plugins/api-tokens.md) | 令牌权限范围、续期 / 加流量接口、幂等键 |
| [插件扩展订阅](plugins/subscription.md) | 插件提供的订阅格式、订阅改写和外部指标接口 |
| [插件示例](examples/plugins) | 本仓库附带的最小示例 |
| [polaris-plugins](https://github.com/cuijianzhuang/polaris-plugins) | 插件 SDK、清单规范、示例插件，以及 CDN 优选 IP 测速程序 cf-ip-probe |

## 设计说明

设计说明记录各功能的实现方式、边界和验证方法，面向开发者和需要了解细节的管理员。日常操作以用户手册为准；两者不一致时，以代码和用户手册为准。

### 节点、内核与运维

| 文档 | 内容 |
| --- | --- |
| [节点小批升级](design/staged-upgrade.md) | Agent / 内核按批升级、观察期、失败停止和本机试运行回退 |
| [每节点探针策略](design/node-probe-policy.md) | 测试点继承、独立、关闭 |
| [节点双栈回程检测](design/node-route-trace.md) | traceroute 回程、定时检测、环境检查 |
| [回程的本地 ASN 归属](design/route-asn.md) | ASN 数据库的获取、上传与启用 |
| [回程国家、省份、城市归属](design/route-geo.md) | 城市数据库与地理注释 |
| [探针大盘优化](design/probe-monitor.md) | 探针页与公开状态页的界面设计 |
| [运维通知策略](design/notification-policy.md) | 事件 × 渠道、合并窗口、维护静默 |
| [概览运维异常汇总](design/operational-overview.md) | 仪表盘「运维汇总」的统计口径 |
| [管理界面：运维与中转功能接入](design/operations-ui.md) | 各运维功能的界面入口与接口对照 |
| [服务器网卡流量与成本统计](design/server-network-accounting.md) | 服务器成本页的统计方式 |

### 入站、路由与中转

| 文档 | 内容 |
| --- | --- |
| [入站模板](design/inbound-templates.md) | 预览、应用与同步 |
| [REALITY 目标发现与复核](design/reality-scanner.md) | 目标扫描与节点侧检测 |
| [Snell 独立用户接入](design/snell.md) | 多用户与独立 PSK 两种模式 |
| [按入站设置出站 IP 策略](design/outbound-ip-strategy.md) | 自动 / 仅 IPv4 / 仅 IPv6 / 优先 IPv4 / 优先 IPv6 |
| [均衡器之间回退](design/balancer-fallback.md) | 主组失败时切到备用组 |
| [入站一键中转与转发](design/relay-one-click.md) | 原样转发与认证隧道、多入口、诊断 |
| [中转变更影响预览](design/relay-change-preview.md) | 启停、删除前的影响清单 |

### 用户、订阅与计费

| 文档 | 内容 |
| --- | --- |
| [客户独立限速](design/per-user-speed-limit.md) | Agent 限速转发（专业版） |
| [端口转发商品化](design/forward-shop.md) | 套餐内的用户自助转发（专业版） |
| [可复用订阅处理组](design/sub-processing-groups.md) | 订阅转换的共享处理步骤（专业版） |
| [外部订阅节点资源与服务器引用](design/subscription-node-resources.md) | 外部节点的稳定标识与引用 |
| [用户流量采集与来源排查](design/user-traffic-attribution.md) | “没用却有流量”的排查方法 |
| [订单、计费与备份的故障一致性](design/transaction-durability.md) | 订单快照、交付与恢复 |

### 账号、授权与界面

| 文档 | 内容 |
| --- | --- |
| [管理员通行密钥与账号恢复](design/passkeys.md) | Passkey 注册、登录、撤销与恢复码 |
| [授权管理（许可证）](design/licensing.md) | 社区版 / 专业版、联网激活与离线许可证 |
| [插件系统设计](design/plugins.md) | 插件框架的分阶段设计 |
| [设置工作流与移动端完善](design/settings-workflows.md) | 设置页的未保存保护等交互 |
| [控制台 UI 打磨与主题方向](design/ui-refresh.md) | 布局与主题 |

### 对照分析

| 文档 | 内容 |
| --- | --- |
| [FLYX 与 Polaris 功能对照](design/flyx-feature-comparison.md) | 2026-10-24 的功能对照与改进建议；其中列为待办的通行密钥、小批升级、转发商品等已陆续实现，以上方各专题为准 |

## 参与开发

- [贡献指南](../CONTRIBUTING.md)：开发环境、提交前检查、PR 约定
- [安全策略](../SECURITY.md)：如何私下报告漏洞
