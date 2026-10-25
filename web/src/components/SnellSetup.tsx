import { useState } from 'react'
import { Link } from 'react-router-dom'
import { get } from '../api'
import { t } from '../i18n'
import type { Node } from '../types'
import { ErrorNote, Field, Modal, useLoad } from './ui'

type Ready = { ready: boolean; minVersion: string; installedVersion: string; reasons: string[] }
export default function SnellSetup({ nodes, initialNode, onClose, onCreate }: { nodes: Node[]; initialNode?: string; onClose: () => void; onCreate: (nodeId: string) => void }) {
  const [nodeId, setNodeId] = useState(initialNode || nodes.find((n) => n.cores.some((c) => c.type === 'singbox'))?.id || nodes[0]?.id || '')
  const ready = useLoad(() => nodeId ? get<Ready>(`/nodes/${nodeId}/snell-readiness`) : Promise.resolve(null), [nodeId])
  return <Modal title={t('Snell 入站')} onClose={onClose} footer={<>
    <button className="btn" type="button" onClick={onClose}>{t('取消')}</button>
    <button className="btn pri" type="button" disabled={ready.loading || !ready.data?.ready} onClick={() => onCreate(nodeId)}>{t('配置 Snell 入站')}</button>
  </>}>
    <div className="note">{t('Snell 入口位于入站的协议选择中。Surge 请使用独立 PSK / 端口模式；无需另装官方 Snell 服务端。')}</div>
    <Field label={t('节点')} htmlFor="snell-setup-node"><select id="snell-setup-node" className="input" value={nodeId} onChange={(e) => setNodeId(e.target.value)}>{nodes.map((n) => <option key={n.id} value={n.id}>{n.name}</option>)}</select></Field>
    <ErrorNote error={ready.error} />
    {ready.data?.ready ? <div className="note">{t('节点已满足 Snell 的 Agent、内核和用户计费要求。')}</div> : <>
      <ul>{(ready.data?.reasons ?? []).map((reason) => <li key={reason}>{reason}</li>)}</ul>
      <div className="hint">{t('能力尚未就绪时不会创建无法使用的入站；升级后可重新检查。')}</div>
    </>}
    <div className="row"><Link className="btn" to={'/cores?node=' + encodeURIComponent(nodeId)}>{t('内核管理')}</Link><Link className="btn" to="/nodes">{t('节点管理')}</Link><button className="btn" disabled={ready.loading} onClick={ready.reload}>{t('重新检查')}</button></div>
  </Modal>
}
