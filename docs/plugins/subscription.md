# 插件扩展订阅

插件可以给订阅增加自定义的客户端格式。本页说明宿主（Polaris）这一侧的接口；SDK 和示例在 [polaris-plugins](https://github.com/cuijianzhuang/polaris-plugins)。

## 自定义订阅格式

### 在清单中声明

```json
{
  "id": "my-format",
  "name": "我的格式",
  "version": "1.0.0",
  "subscription": {
    "formats": [
      { "name": "myclient", "title": "MyClient 配置", "contentType": "text/plain" }
    ]
  }
}
```

| 字段 | 规则 |
| --- | --- |
| `name` | 订阅地址里的 `?format=` 值。小写字母开头，可以有数字和 `-`，2–24 个字符。不能使用内置格式名（`links`、`clash`、`singbox`、`surge`、`loon`、`quanx`、`html` 等），也不能与已安装的其他插件重名。 |
| `title` | 在后台显示的名称，最长 40 个字符。 |
| `contentType` | 响应只能使用这个类型，可选值为 `text/plain`、`text/yaml`、`application/yaml`、`application/x-yaml`、`application/json`、`application/xml`、`text/xml`。不允许 HTML 或脚本。 |

每个插件最多声明 5 个格式。

安装或升级时，管理员会看到这些格式，以及「会收到用户节点和连接凭据」的提示。新版本新增格式时需要管理员重新批准。插件停用或卸载后，它的格式立即失效，访问会返回 400；其他格式不受影响。

### 渲染请求

用户请求 `/sub/<令牌>?format=myclient` 时，面板会：

1. 照常检查账号状态；
2. 计算该用户可用的节点（与内置格式相同，包括节点命名、筛选和信息节点）；
3. 向插件发送一个签名请求：

```
POST {插件地址}/polaris/render
Content-Type: application/json
X-Polaris-Signature: t=<unix 秒>,v1=<HMAC-SHA256(插件密钥, "<t>.<原始正文>") 的十六进制>
```

请求正文：

```json
{
  "format": "myclient",
  "user": { "username": "alice", "title": "Polaris", "upload": 0, "download": 0, "total": 0, "expire": null, "updateHours": 1 },
  "nodes": [ { "Name": "东京", "Server": "203.0.113.9", "Port": 443, "Protocol": "vless", "UUID": "…", "Transport": {…}, "TLS": {…} } ]
}
```

`nodes` 的字段与 Polaris 内部的 `sub.Node` 相同，**包含 UUID、密码等连接凭据**。插件必须先验证签名，并且不能保存或转发这些凭据。

### 响应要求

- 状态码 200，`Content-Type` 与清单声明的类型一致（可以带 `charset`）。
- 3 秒内完成响应，正文不超过 4 MiB。
- 不满足要求、超时或插件不可用时，用户会收到 503「插件订阅格式暂不可用」。

面板会补上订阅标题、流量信息（`subscription-userinfo`）等响应头，并加 `X-Content-Type-Options: nosniff`。

### 缓存

成功的响应缓存 60 秒。缓存 key 包括插件安装、插件版本、格式、用户及其订阅令牌，以及发送给插件的节点数据。因此节点、授权变化或重置订阅令牌后都会重新请求插件；流量数字最多会滞后 1 分钟。
