# Cloudflare Tunnel（Argo Tunnel）回源

流量路径：客户端 → Cloudflare HTTPS 443（可使用优选 IP）→ 命名 Tunnel → 同机 cloudflared → `http://127.0.0.1:入站端口` → 代理出口。

此功能接入你已创建的 Tunnel。可选择手动运行 cloudflared，或让 Agent 安装并托管。JSON 凭据或远程 Token 仅在启用 Agent 托管时加密保存；不会把密钥写入配置历史、订阅或日志。Tunnel 与付费 Argo Smart Routing 是不同功能。

隧道通过主动向 Cloudflare 建立连接来隐藏回源公网入口，不改变代理出口 IP。客户端仍需能访问 Cloudflare 边缘与入口域名；Tunnel 不能保证避免 GFW 的 DNS、SNI 或 IP 阻断。

## 支持范围

- 本次支持 WS 入站，客户端 HTTPS 443，同机 `127.0.0.1` 明文 HTTP 回源。客户端 TLS 在 Cloudflare 终止，隧道本身加密。REALITY、原生 TCP/UDP、Hysteria2、TUIC 和 gRPC/XHTTP/HTTPUpgrade 不在此模式当前支持范围内。
- 每个 Tunnel UUID 对应一个配置档，配置档只关联一台节点。不要把同一 UUID 的连接器部署到多台业务配置不同的 VPS，否则 Cloudflare 可能把请求分配给错误节点。
- Tunnel 入站不能混用直连、中转或其他 CDN 配置档，不能启用 PROXY protocol、Caddy 统一回源或 Extra 覆盖。模板批量部署暂不支持 Tunnel，请通过入站页面创建或「创建 CDN 入站」生成。
- 面板与订阅接口仍可使用原有优选 IP、固定域名、地区/运营商选择和定时检测。检测结果仍来自面板网络，仅代表 DNS/TCP/TLS/WS 握手，不代表用户网络或完整代理业务成功。

## 配置流程

### 一键安装（Cloudflare 远程 Token）

1. 同时更新面板和 Agent，等待节点上报 `cloudflare-tunnel-v1` 和 `cloudflare-tunnel-token-v1`。只支持 JSON 的旧 Agent 不能获取远程 Token 配置。
2. 在 Cloudflare 创建远程管理的命名 Tunnel，取得 Tunnel UUID 和安装命令中的完整 Token（不是 Cloudflare API Token）。在面板创建 Tunnel CDN 配置档，填写对应 UUID 和公开入口域名。
3. 打开该配置档的「Agent 托管」，开启托管，选择「远程 Token（Cloudflare 维护路由）」、目标节点，粘贴 Token，点「一键安装并启动」。不需要手动 SSH 安装 cloudflared。Agent 会校验官方安装包（默认 2026.10.7，Token 文件模式最低 2025.4.0）、私密保存 Token，并托管连接器；即使尚未创建入站，也可以先安装并建立边缘连接。
4. 为该节点创建使用此配置档的本机 WS 入站。按「隧道回源配置」表格，在 Cloudflare 配置公开主机名、路径、Service 和 HTTP Host Header。云端路由是实际生效的配置；面板显示期望值，**不会自动推送远程路由**，也不会创建 Cloudflare 隧道或代替账户授权。
5. 等待「节点隧道连接已就绪」，再同步 DNS、检测真实入口并更新订阅。连接器就绪只代表连接到 Cloudflare，不保证远程路由正确或业务可用。更改入站路径、端口或 Host 后需同步修改 Cloudflare 路由。

Token 在面板加密保存，只通过绑定节点的认证通道下发。Agent 将原始 Token 写入 `<数据目录>/run/cloudflared/credentials/<配置档>/<版本>.token`（`0600`），通过 `--token-file` 启动，不把 Token 放进命令行、环境变量、生成的 YAML、配置历史或读接口。重新打开窗口不会回显 Token，留空保存保留当前值；填写新 Token 会轮换私有文件并重启对应连接器。Token 必须匹配配置档的 UUID，不要粘贴整条安装命令。

停用后的停止、清理和等待确认流程与本地 JSON 模式相同。切换管理方式前需停用并等待旧节点确认清理，再导入新方式的凭据。

### Agent 托管（本地管理的命名隧道）

1. 先同时更新面板和 Agent，等待节点连接并上报 `cloudflare-tunnel-v1` 能力。旧 Agent 会在获取配置时被明确拦截。
2. 在自己可信的管理机器上用 Cloudflare 官方工具创建本地管理的命名隧道（`cloudflared tunnel login`、`cloudflared tunnel create <名称>`），取得 UUID 和对应凭据 JSON。Cloudflare 控制台生成的远程 Token 不能替代此 JSON。
3. 在 CDN 配置档选择 `Cloudflare Tunnel (Argo)`，填写 UUID。为对应节点创建本机 WS 入站，再打开「Agent 托管」，选择节点并导入凭据 JSON。面板不代替用户登录 Cloudflare 账户或创建隧道。
4. Agent 下载并校验官方 cloudflared（默认 2026.10.7），保存私有凭据，自动生成路由并运行连接器。每个隧道独立运行，崩溃后退避重启，Agent 退出/崩溃时其子进程也会停止。重启 Agent 时可使用本机私有缓存恢复服务。
5. 在托管窗口等待「节点隧道连接已就绪」，然后同步 DNS、检测入口并更新订阅。运行中不等于就绪：状态通过本机 `/ready` 检查，并在窗口每 5 秒刷新；多隧道节点显示该节点连接器的汇总状态。

改变 WS 路径、端口或 Host 后，正常发布配置即可由 Agent 更新路由；只有变化的连接器需要重启。重新导入凭据会生成新的凭据版本并重启对应连接器，已有 WS 连接可能中断。最多支持每节点 32 个托管隧道，每隧道 1024 条路由。

面板只在经节点认证的配置获取通道下发**当前**凭据；历史配置只保存凭据版本，轮换或停用后旧版本不能重新获取密钥。Agent 在 `<数据目录>/run/cloudflared/credentials/<配置档>/<版本>.json` 中以 `0600` 权限保存密钥，目录为 `0700`。连接器不继承 `TUNNEL_*` 环境变量的令牌、日志文件或回源覆盖，原有代理和 CA 环境保留；其输出只用于归类安全的故障原因，不保留原始日志。

停用托管会删除面板中的当前凭据，由 Agent 下次同步停止进程并删除本机副本。仅当文件清理成功才确认该配置已应用；清理失败会保留待处理状态并退避重试。离线节点显示等待清理，不能冒充已完成。更换节点或切换 UUID/回源方式前，必须等待旧节点确认清理；迁移节点时还需移除旧节点关联的入口。停用不是在 Cloudflare 全局吊销凭据：丢失节点或凭据泄露时，需要在 Cloudflare 撤销/更换授权。加密备份仍可能包含过去的加密凭据，恢复旧备份不等于恢复 Cloudflare 已撤销的授权。

### 自行运行 cloudflared

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
