# Cloudflare Tunnel（Argo Tunnel）回源

流量路径：客户端 → Cloudflare HTTPS 443（可使用优选 IP）→ 命名 Tunnel → 同机 cloudflared → `http://127.0.0.1:入站端口` → 代理出口。

此功能接入你已创建的 Tunnel。Polaris 保存公开的 Tunnel UUID、生成回源配置并同步 DNS，不保存连接令牌或凭据文件，也不安装、启动 cloudflared。Tunnel 与付费 Argo Smart Routing 是不同功能。

隧道通过主动向 Cloudflare 建立连接来隐藏回源公网入口，不改变代理出口 IP。客户端仍需能访问 Cloudflare 边缘与入口域名；Tunnel 不能保证避免 GFW 的 DNS、SNI 或 IP 阻断。

## 支持范围

- 本次支持 WS 入站，客户端 HTTPS 443，同机 `127.0.0.1` 明文 HTTP 回源。客户端 TLS 在 Cloudflare 终止，隧道本身加密。REALITY、原生 TCP/UDP、Hysteria2、TUIC 和 gRPC/XHTTP/HTTPUpgrade 不在此模式当前支持范围内。
- 每个 Tunnel UUID 对应一个配置档，配置档只关联一台节点。不要把同一 UUID 的连接器部署到多台业务配置不同的 VPS，否则 Cloudflare 可能把请求分配给错误节点。
- Tunnel 入站不能混用直连、中转或其他 CDN 配置档，不能启用 PROXY protocol、Caddy 统一回源或 Extra 覆盖。模板批量部署暂不支持 Tunnel，请通过入站页面创建或「创建 CDN 入站」生成。
- 面板与订阅接口仍可使用原有优选 IP、固定域名、地区/运营商选择和定时检测。检测结果仍来自面板网络，仅代表 DNS/TCP/TLS/WS 握手，不代表用户网络或完整代理业务成功。

## 配置流程

1. 在 Cloudflare 创建命名 Tunnel，取得 UUID，并在对应 VPS 安装 cloudflared。使用自己的 Cloudflare DNS 域名；不是临时 `trycloudflare.com` Quick Tunnel。
2. 面板「链路与 CDN → CDN 配置档 → 新建/编辑」选择回源方式 `Cloudflare Tunnel (Argo)`，填写 UUID、公开入口域名。客户端协议和端口固定为 HTTPS 443。已有公网回源入站不能直接切换：先建立独立的 Tunnel WS 入站。
3. 在该节点创建 VLESS + WS 入站，选择这个 CDN 配置档并只走 CDN。页面「一键配置 CDN」会选本机空闲端口、监听 `127.0.0.1` 并关闭入站 TLS；再次修改这些设置会触发服务端校验。也可以从已有 REALITY 入站创建独立 CDN WS 入站，原入站保持独立。
4. 配置档点击「隧道回源配置」，根据隧道管理方式设置 cloudflared：
   - 本地管理：将导出的 YAML 保存为 `/etc/cloudflared/config.yml`，将相应凭据 JSON 保存在导出配置指定的路径，仅允许管理员读取。验证后运行 `cloudflared tunnel --config /etc/cloudflared/config.yml run`，并按 Cloudflare 官方方式配置服务自启。
   - 远程管理：在 Cloudflare 后台按表格配置公开主机名、路径、Service 和 HTTP Host Header，使用 Cloudflare 提供的连接命令/服务令牌。远程管理模式不读取导出的本地 YAML。
5. 确认连接器在线后，在面板点「同步 DNS」，或打开自动同步。需要 Zone 读取和 DNS 编辑权限，不需要面板持有 Tunnel 管理权限。DNS 指向 `<UUID>.cfargotunnel.com`，不会指向 VPS IP，也不会修改整个 Zone 的 SSL 模式或签发 Origin 证书。
6. 执行入口/优选地址检测，再更新订阅。定时检测可以持续检查失败地址；明确失败后排除，通过后恢复。

生成的路径是精确匹配 WS 路径的正则表达式，最后一条规则固定为 `http_status:404`。同一 Host 与路径不能路由到不同入站端口。更改入站端口、Host 或路径后，必须重新导出并更新本地配置，或更新 Cloudflare 的远程路由；面板不会自动推送这些外部配置。

cloudflared 必须能访问同一宿主机的回环监听。Docker 使用 host 网络或其他能访问宿主机回环的部署方式，不要为了连接容器而开放公网入站端口。VPS 需要允许 Cloudflare Tunnel 要求的出站连接（当前使用 7844/TCP 或 UDP）；不需要为这个业务入站开放公网入站端口。

## DNS 迁移和删除

旧 A/AAAA 记录与 Tunnel CNAME 不能共存。同步只会迁移指向本节点已知 IP 的旧记录，遇到其他网站的地址会拒绝修改。迁移写入失败时尝试恢复原 A/AAAA 记录；若恢复也失败，会明确返回错误，需要在 Cloudflare 核实记录。DNS 修改本身不能保证零中断，应先确认 Tunnel 路由和连接器已就绪。

删除最后一个隧道入口会停用对应入站，避免无入口时订阅默认生成直连节点。删除配置档沿用现有的停用规则。停用后若要再次使用，先恢复有效的 Tunnel 入口，再启用入站。

Xray 的真实 IP 头仅在通过校验的本机回环 Tunnel 入站启用，公网监听继续忽略客户端伪造的转发头。同机进程属于此回源信任边界。其他内核的真实 IP 采集能力仍受其实现限制，不会伪造地址或延长已过期的在线 IP。

参考：[Cloudflare Tunnel](https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/) · [支持的服务协议](https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/routing-to-tunnel/protocols/)。
