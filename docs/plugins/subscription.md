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

## 改写订阅节点

插件可以在每次生成订阅前，对该用户的节点做**筛选、改名、排序**。这对所有格式都生效，包括内置格式和插件格式。

### 声明

```json
{ "subscription": { "transform": true } }
```

安装时管理员会看到「改写订阅节点」。如果新版本新开这项能力，需要管理员重新批准。

### 请求

```
POST {插件地址}/polaris/transform
X-Polaris-Signature: t=…,v1=…   （签名方式同上）
```

```json
{
  "format": "clash",
  "user": "alice",
  "nodes": [
    { "id": "0", "name": "东京 VLESS", "region": "JP", "source": "panel", "protocol": "vless", "server": "203.0.113.9", "port": 443 },
    { "id": "1", "name": "香港 Trojan", "region": "HK", "protocol": "trojan", "server": "198.51.100.7", "port": 443 }
  ]
}
```

插件**看不到** UUID、密码等连接凭据。

### 响应

```json
{ "nodes": [ { "id": "1", "name": "🇭🇰 香港" }, { "id": "0" } ] }
```

- 返回的顺序就是新的顺序。没有列出的节点会被去掉。
- `name` 可省略，省略表示不改名。改名最长 100 个字符，不能包含控制字符。
- 只能引用请求中已有的 `id`，且每个 `id` 只能出现一次。插件不能新增节点；节点的地址、端口和凭据都由面板保留，插件改不了。
- `{"nodes": []}` 是合法的，表示全部过滤掉。

### 失败与顺序

- 每个插件限时 2 秒，所有改写插件合计不超过 4 秒。
- 遇到以下情况，面板会跳过这个插件，节点列表保持它处理前的样子：
  - 超时；
  - 状态码不是 200；
  - 返回的不是 `{"nodes": [...]}`；
  - 引用了不存在或重复的 `id`；
  - 名称不合法。
- 有多个改写插件时，按管理员在「扩展 → 插件」里设置的顺序依次处理，后一个插件拿到的是前一个处理后的结果。未设置顺序的插件排在最后，按插件 ID 排序。
- 改写结果缓存 60 秒，缓存 key 包括插件的安装与版本，以及发给插件的完整请求内容。

## 上报节点外部指标

监控类插件可以把自己测到的数据上报到节点上，例如「从上海电信测到的延迟」。这些数据在节点详情的「外部指标」卡片里显示，并标明来源。它们**不会**影响节点的在线状态、心跳、CPU、内存或流量。

### 权限

令牌需要 `nodes:metrics:write` 权限。这个权限只能上报指标：不能读取节点列表，也不能修改节点。插件在清单的 `scopes` 里申请这个权限，安装时由管理员批准。

### 请求

```
POST /api/v1/nodes/{id}/metrics
Authorization: Bearer plt_…
```

```json
{
  "metrics": [
    { "name": "latency.sh-ct", "label": "上海电信", "unit": "ms", "value": 38.5 },
    { "name": "speed", "unit": "Mbps", "value": 920, "at": "2026-10-26T08:00:00Z" }
  ]
}
```

| 字段 | 规则 |
| --- | --- |
| `name` | 小写字母开头，可以有数字和 `_` `.` `-`，最长 40 个字符。`cpu`、`mem`、`online`、`status`、`traffic` 等名称保留给节点自身的数据，不能使用。 |
| `label` | 可选，显示用，最长 40 个字符，不能有控制字符。 |
| `unit` | 可选值：`ms`、`s`、`Mbps`、`kbps`、`%`、`count`、`bool`。单位为 `bool` 时，`value` 只能是 0 或 1。 |
| `value` | 有限数字，绝对值不超过 10¹²。 |
| `at` | 可选，RFC 3339 格式，必须是最近 24 小时内（最多比当前时间晚 5 分钟）。不填表示当前时间。 |

- 每次最多 20 个指标，同一请求里名称不能重复。
- 每个节点最多保存 100 个外部指标，同名指标会覆盖旧值。
- 每个指标按「来源 + 名称」保存最新一次的值。插件自己的令牌，来源记为插件 ID；其他令牌记为 `token:<令牌名>`。
- 超过 30 天未更新的指标会自动清理；删除节点时，它的外部指标也一并删除。

### 读取

`GET /api/v1/nodes/{id}/metrics/external` 需要 `nodes:read` 权限，返回该节点的全部外部指标。
