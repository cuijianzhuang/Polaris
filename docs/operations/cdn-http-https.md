# CDN 的 HTTP 与 HTTPS 节点

在「链路与路由 → CDN 配置档」中新建或编辑配置档，选择「CDN 协议」。该选项控制客户端到 CDN 的连接，独立于 Cloudflare 的回源 SSL 模式。既有配置和不带此字段的旧备份保持 HTTPS；编辑时旧 API 省略该字段会保留当前值。

| CDN 协议 | Cloudflare 支持的端口 | 订阅连接 |
| --- | --- | --- |
| HTTP | 80、8080、8880、2052、2082、2086、2095 | 不使用 TLS，保留入口的 HTTP Host 和路径 |
| HTTPS | 443、2053、2083、2087、2096、8443 | 使用 TLS，入口域名作为 SNI 和 HTTP Host |

切换 CDN 协议后，表单会重置可用端口。可调整端口顺序，第一个是默认连接端口。入站使用该 CDN 协议支持的 Cloudflare 端口时，订阅保持入站原端口；入口显式指定的端口同样按所选 HTTP/HTTPS 端口集合校验。

HTTP 客户端通过 Cloudflare 时，回源也使用 HTTP，关联入站应关闭 TLS。HTTPS 客户端的回源由 Full / Full (strict) / Flexible 决定。HTTP 配置档同步 DNS 只维护 DNS 记录，不修改整个域名的 SSL 模式，也不签发未使用的 Origin 证书。同一域名需要同时提供 HTTP 与 HTTPS 节点时，可以分别建立配置档。

- Cloudflare 的 gRPC 入口要求 **HTTPS + 443**。HTTP 可用于面板支持的 WS、HTTPUpgrade、XHTTP 传输。
- 内置 Caddy 统一回源目前使用 TLS，HTTP 配置档会关闭此选项。
- HTTP 节点依赖不被 Cloudflare 的 Always Use HTTPS 或其他规则重定向。入口检测使用 HTTP 握手，并在被转向 HTTPS 时给出原因。
- 一键配置及新建 CDN 入站会按配置档选择端口与回源安全方式；模板自动分配端口也识别所选方式。

测速结果只在相同 CDN 协议（HTTP/HTTPS）的 Cloudflare 配置档间共享。切换有测速点的配置档前，需先移除测速点及上报结果，避免将旧协议的测速结果带到新协议中；关联入站的回源安全方式不匹配时也会拒绝切换，可新建配置档并调整入口。

当前 `cf-ip-probe` 一键安装程序只验证 HTTPS，因此 HTTP 配置档不提供其安装入口。HTTP 配置档可手动维护优选域名和地址、设置来源 URL，或使用 `cdn:ips:write` API 令牌自行按 HTTP 端口测量并上报。面板已有的节点可达性检查使用对应配置档的默认端口。

API 字段是 `scheme: "http" | "https"`，适用于创建及完整编辑 CDN 配置档。入口的 `target.scheme` 会返回实际 CDN 协议，订阅、检测与页面展示使用同一规则。

参考：[Cloudflare 支持的端口](https://developers.cloudflare.com/fundamentals/reference/network-ports/)；[Full 模式按客户端协议回源](https://developers.cloudflare.com/ssl/origin-configuration/ssl-modes/full/)。
