# CDN 真实 IP 的信任边界

Xray 的 `trustedXForwardedFor` 按请求头是否存在决定是否读取 X-Forwarded-For，并不验证 TCP 对端。公开监听不能因为存在 CF-Connecting-IP 就信任转发头：直连回源的代理客户端也能构造相同请求头。该配置为空时，部分 Xray HTTP 传输反而会默认读取所有 XFF。

面板和 Agent 现在采用以下规则：

- 公开的 WS、HTTPUpgrade、XHTTP、gRPC 监听不信任请求头；Xray 显式使用 `[""]` 阻止默认 XFF 回退，空名字无法成为合法 HTTP 请求头。
- 使用 Agent 内置 Caddy 前置的入站由面板移到 loopback。Caddy 根据实际连接对端匹配 Cloudflare 网段，只有可信对端提供的 CF-Connecting-IP 才可作为访客地址，忽略客户端自带的 XFF 链。
- Caddy 删除传入 CF 头，并用验证后的地址覆盖 XFF 和内部标记 X-Polaris-Client-IP。只有受保护的 loopback Xray 入站接受该标记。
- 前置被端口占用、证书缺失等原因跳过时，不留下请求头信任。其他本机进程属于同一服务器的信任边界。

应同时更新面板和 Agent，确认配置版本已应用。内置 Caddy 前置的来源清洗以 Caddy 2.10.2 验证；CI 下载固定版本并校验 SHA-256，实际运行正反向测试。旧 Agent 若尚不支持转发头控制，不能仅靠更新面板获得这里的安全保证。

未启用内置 Caddy 前置、直接接受 CDN 回源的入站会记录边缘连接地址，无法可靠区分访客；Cloudflare 边缘地址仍不计入用户 IP 数限制。需要可靠访客 IP 时请启用受保护的前置，不应恢复公开监听的头信任。手写 native 配置不由这里的渲染规则保证。

验证覆盖真实 Caddy 上不可信对端伪造全部头、测试可信代理的访客 IP 保留，以及真实 Xray VLESS/WS 的实际目标连通与伪造地址拒绝。WS、HTTPUpgrade、XHTTP、gRPC 的公开监听渲染均有断言；这些测试不等于每种协议都做过完整线上 CDN 验收。
