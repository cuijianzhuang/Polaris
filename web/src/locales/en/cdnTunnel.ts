export default {
  '隧道回源配置': 'Tunnel origin configuration',
  '回源方式': 'Origin connection',
  '公网回源': 'Public origin',
  '复制配置': 'Copy configuration',
  '填写已有命名隧道的 UUID，不是 Token。每个配置档只关联同一台节点，cloudflared 由你在该节点运行。': 'Enter an existing named tunnel UUID, not its token. Each profile serves one node. Run cloudflared on that node yourself.',
  '当前支持 WS、HTTPS 443 和同机回源。入站需监听 127.0.0.1 并关闭 TLS；优选 IP 仍可使用。隧道隐藏回源入口，不改变出口 IP，也不能保证域名或连接不被阻断。': 'Supports WS, HTTPS 443 and a local origin. Listen on 127.0.0.1 with inbound TLS disabled; preferred IPs still work. The tunnel hides the origin entry, does not change the exit IP, and cannot guarantee that the domain or connection will remain reachable.',
  '自动同步隧道 DNS：把入口域名指向 Tunnel，不写入 VPS IP，不修改域名 SSL 模式。': 'Automatically point entry DNS records to the tunnel without publishing the VPS IP or changing the zone SSL mode.',
  '在对应节点运行 cloudflared。面板不保存隧道密钥，也不会创建或启动隧道。先完成隧道连接和回源配置，再同步 DNS 并检测入口。': 'Run cloudflared on the corresponding node. The panel does not store connector credentials or create or start tunnels. Connect the tunnel and configure origin routing before syncing DNS and checking the entry.',
  '本地管理的隧道：将下方内容保存为 /etc/cloudflared/config.yml，凭据 JSON 仅保存在该节点。配置变更后需自行重新加载 cloudflared。': 'Locally managed tunnels: save this as /etc/cloudflared/config.yml and keep the credential JSON on this node only. Reload cloudflared yourself after configuration changes.',
  '远程管理的隧道：在 Cloudflare 后台按下表设置公开主机名和路径，HTTP Host Header 填对应值；远程 Token 隧道不使用上面的本地配置文件。': 'Remotely managed tunnels: set the public hostname, path and HTTP Host Header in Cloudflare using this table. Token-based remote tunnels do not use the local configuration file above.',
  '此路由仅供同机 cloudflared 使用。Docker 部署需能访问宿主机回环地址；不要通过开放公网入站端口绕过此限制。': 'These routes require cloudflared on the same host. Docker deployments must reach the host loopback address; do not expose public inbound ports to work around this requirement.',
  'Tunnel 已选：使用 WS、127.0.0.1 明文回源，并且只发布 CDN 入口。客户端使用 HTTPS 443。': 'Tunnel selected: use WS with a plain HTTP origin on 127.0.0.1 and publish only the CDN entry. Clients use HTTPS 443.',
}
