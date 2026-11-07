import { get } from '../api'
import { t } from '../i18n'
import { copyText, Modal, useLoad, useToast } from './ui'

type TunnelConfig = { nodeId: string; tunnelId: string; dnsTarget: string; config: string; routes: { hostname?: string; path?: string; service: string; originRequest?: { httpHostHeader: string } }[] }

export default function CDNTunnelConfig({ profileId, managed, credentialMode, onClose }: { profileId: string; managed?: boolean; credentialMode?: string; onClose: () => void }) {
  const data = useLoad((signal) => get<TunnelConfig>(`/cdn-profiles/${profileId}/tunnel-config`, signal), [profileId])
  const toast = useToast()
  return <Modal title={t('隧道回源配置')} onClose={onClose} footer={<button className="btn" type="button" onClick={onClose}>{t('关闭')}</button>}>
    <p className="hint">{credentialMode === 'remote' ? t('远程 Token 的路由由 Cloudflare 下发。下表仅为面板期望值，需要在 Cloudflare 配置；Agent 不会推送这些规则。') : managed ? t('此隧道由 Agent 自动维护配置和进程，无需手动部署以下路由。请在「Agent 托管」中查看应用和连接状态。') : t('在对应节点运行 cloudflared。面板不保存隧道密钥，也不会创建或启动隧道。先完成隧道连接和回源配置，再同步 DNS 并检测入口。')}</p>
    {data.error && <div className="note bad" role="alert">{data.error}<button className="link" type="button" onClick={data.reload}>{t('重试')}</button></div>}
    {data.data && <>
      <p className="small mono" style={{ overflowWrap: 'anywhere' }}>{data.data.dnsTarget}</p>
      {!managed && credentialMode !== 'remote' && <><p className="hint">{t('本地管理的隧道：将下方内容保存为 /etc/cloudflared/config.yml，凭据 JSON 仅保存在该节点。配置变更后需自行重新加载 cloudflared。')}</p>
      <pre className="code" style={{ maxHeight: 320, overflow: 'auto' }}>{data.data.config}</pre>
      <button className="btn sm" type="button" onClick={async () => { await copyText(data.data!.config); toast(t('已复制')) }}>{t('复制配置')}</button>
      <p className="hint">{t('远程管理的隧道：在 Cloudflare 后台按下表设置公开主机名和路径，HTTP Host Header 填对应值；远程 Token 隧道不使用上面的本地配置文件。')}</p></>}
      <div className="scroll"><table><thead><tr><th>Host / Path</th><th>Service / HTTP Host Header</th></tr></thead><tbody>{data.data.routes.filter((r) => r.hostname).map((r) => <tr key={r.hostname! + r.path}><td className="mono">{r.hostname}<br />{r.path}</td><td className="mono">{r.service}<br />{r.originRequest?.httpHostHeader}</td></tr>)}</tbody></table></div>
      <p className="hint">{t('此路由仅供同机 cloudflared 使用。Docker 部署需能访问宿主机回环地址；不要通过开放公网入站端口绕过此限制。')}</p>
    </>}
  </Modal>
}
