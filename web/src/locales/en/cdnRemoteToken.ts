export default {
 '一键安装并启动': 'Install and start',
 '隧道管理方式': 'Tunnel management mode',
 '本地 JSON（Agent 维护路由）': 'Local JSON (Agent manages routes)',
 '远程 Token（Cloudflare 维护路由）': 'Remote token (Cloudflare manages routes)',
 'Agent 自动安装并校验 cloudflared，维护连接器进程。选择本地 JSON 或远程 Token；远程回源规则仍由 Cloudflare 管理。': 'The Agent installs and verifies cloudflared and supervises the connector. Choose local JSON or a remote token; Cloudflare manages remote origin rules.',
 'Token 已加密保存，留空保留；填写新 Token 会重启此连接器。': 'The token is stored encrypted. Leave blank to retain it; a new token restarts this connector.',
 '只粘贴完整 Token，不要粘贴安装命令。Token 所属 UUID 必须与当前配置档一致。': 'Paste only the complete token, not the installation command. Its tunnel UUID must match this profile.',
 '一键安装只启动连接器，不会创建 Cloudflare 隧道或修改云端路由。请在 Cloudflare 配置公开域名、路径和本机回源地址，再检测实际入口。': 'Installation starts the connector without creating a Cloudflare tunnel or changing remote routes. Configure the public hostname, path and local origin in Cloudflare, then check the actual entry.',
 '远程 Token 的路由由 Cloudflare 下发。下表仅为面板期望值，需要在 Cloudflare 配置；Agent 不会推送这些规则。': 'Cloudflare supplies remote-token routes. The table shows the panel’s expected values; configure them in Cloudflare. The Agent does not push these rules.',
}
