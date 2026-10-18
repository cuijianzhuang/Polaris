# 插件清单设计示例

插件框架和后续示例已迁至独立仓库 [polaris-plugins](https://github.com/cuijianzhuang/polaris-plugins)。当前协议以新仓库的[完整设计](https://github.com/cuijianzhuang/polaris-plugins/blob/develop/docs/design.md)和[外部清单示例](https://github.com/cuijianzhuang/polaris-plugins/blob/develop/examples/external-notify/polaris-plugin.json)为准。

本目录的 [external-notify/polaris-plugin.json](external-notify/polaris-plugin.json) 保留为设计快照，不是已实现或可直接安装的服务。

`notify-webhook/plugin.json` 是早期宿主内置通知投递器方案的历史样例，使用 `host-webhook-v1` 运行时；它不是当前外部服务协议，不能与 `runtime: external` 混用。
