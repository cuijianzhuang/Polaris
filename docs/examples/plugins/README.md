# 插件清单设计示例

当前方案使用[外部服务清单](external-notify/polaris-plugin.json)，由独立插件服务在 `GET {base}/polaris-plugin.json` 提供。字段与实施计划见[插件系统设计](../../design/plugins.md)。

这些文件是协议设计样例，当前主控还不能安装它们，目录也不包含可运行的插件服务。

`notify-webhook/plugin.json` 是早期宿主内置通知投递器方案的历史样例，使用 `host-webhook-v1` 运行时；它不是当前外部服务协议，不能与 `runtime: external` 混用。
