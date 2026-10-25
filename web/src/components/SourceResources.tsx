import { useState } from 'react'
import { del, get, post } from '../api'
import { t } from '../i18n'
import { datetime } from '../format'
import { Drawer, Empty, ErrorNote, Field, Modal, Switch, useAction, useConfirm, useLoad } from './ui'
import type { Inbound, Node, SourceNode, SourceRefresh } from '../types'

const changes: Record<string, string> = { added: t('新增'), removed: t('移除'), updated: t('变更'), restored: t('恢复') }
const fields: Record<string, string> = { name: t('名称'), endpoint: t('地址'), transport: t('传输'), tls: 'TLS', credentials: t('凭据'), options: t('选项') }

export default function SourceResources({ id, name, onClose }: { id: string; name: string; onClose: () => void }) {
  const nodes = useLoad(() => get<SourceNode[]>(`/sub-sources/${id}/nodes`), [id])
  const history = useLoad(() => get<SourceRefresh[]>(`/sub-sources/${id}/refreshes`), [id])
  const [tab, setTab] = useState<'nodes' | 'history'>('nodes')
  const [removed, setRemoved] = useState(true)
  const [binding, setBinding] = useState<SourceNode | null>(null)
  const { busy, run } = useAction()
  const confirm = useConfirm()
  return <>
    <Drawer wide title={t('来源节点 · {name}', { name })} onClose={onClose}>
      <div className="note">{t('节点使用稳定 ID；改名、重排及可明确匹配的凭据轮换保留引用。移除或停用后阻断相关出站；不会回退直连。')}</div>
      <div className="tabs" role="tablist">
        <button className={'tab' + (tab === 'nodes' ? ' on' : '')} role="tab" aria-selected={tab === 'nodes'} onClick={() => setTab('nodes')}>{t('节点与引用')}</button>
        <button className={'tab' + (tab === 'history' ? ' on' : '')} role="tab" aria-selected={tab === 'history'} onClick={() => { setTab('history'); history.reload() }}>{t('刷新差异')}</button>
      </div>
      <ErrorNote error={nodes.error || history.error} />
      {tab === 'nodes' ? <>
        <div className="toggle"><span>{t('显示已移除节点')}</span><Switch label={t('显示已移除节点')} on={removed} onChange={setRemoved} /></div>
        <div className="scroll"><table>
          <thead><tr><th>{t('节点 / 稳定 ID')}</th><th>{t('地址')}</th><th>{t('状态')}</th><th>{t('出站 / 中转引用')}</th><th>{t('操作')}</th></tr></thead>
          <tbody>{(nodes.data ?? []).filter((n) => removed || !n.missing).map((n) => <tr key={n.id}>
            <td>{n.name}<div className="mono small mute">{n.id}</div><span className="tag">{n.protocol}</span></td>
            <td className="mono small">{n.server}:{n.port}<div className="mute">{[n.network, n.security].filter(Boolean).join(' · ')}</div></td>
            <td>{n.missing ? t('已移除') : !n.available ? t('不可用') : n.supported ? t('可引用') : t('仅订阅输出')}{n.reason && <div className="hint">{n.reason}</div>}</td>
            <td>{n.references.length ? n.references.map((r) => <div key={r.id} className="small"><b>{r.tag}</b> · {r.nodeId}<div className="mute">{t('绑定 {n} 个入站', { n: r.inboundIds.length })}</div></div>) : '—'}</td>
            <td>{n.available && n.supported && <button className="btn sm" disabled={busy} onClick={() => setBinding(n)}>{t('引用')}</button>}
              {n.missing && !n.references.length && <button className="link" disabled={busy} onClick={() => run(async () => { if (await confirm(t('清理已移除节点 {name}？稳定 ID 将不再保留。', { name: n.name }), { danger: true })) { await del('/sub-source-nodes/' + n.id); nodes.reload() } })}>{t('清理')}</button>}
            </td>
          </tr>)}</tbody>
        </table>{!nodes.data?.length && <Empty>{t('没有节点')}</Empty>}</div>
      </> : <>
        <div className="hint">{t('保留最近 100 次成功刷新；只展示变化字段，不展示凭据内容。')}</div>
        {(history.data ?? []).map((h) => <article className="card" key={h.id} style={{ marginTop: 12 }}>
          <b>#{h.revision} · {datetime(h.createdAt)}</b><span className="hint"> · {t('未变更 {n} 个', { n: h.unchanged })}</span>
          <ul>{h.changes.map((c) => <li key={c.id}><span className="tag">{changes[c.kind] ?? c.kind}</span> {c.name} <span className="small mono mute">{c.id}</span>{c.fields?.length ? ' · ' + c.fields.map((f) => fields[f] ?? f).join(' / ') : ''}</li>)}</ul>
          {!h.changes.length && <div className="hint">{t('没有节点变化')}</div>}
        </article>)}
        {!history.data?.length && <Empty>{t('尚无刷新记录')}</Empty>}
      </>}
    </Drawer>
    {binding && <SourceReference node={binding} onClose={() => setBinding(null)} onSaved={() => { setBinding(null); nodes.reload() }} />}
  </>
}

function SourceReference({ node, onClose, onSaved }: { node: SourceNode; onClose: () => void; onSaved: () => void }) {
  const nodes = useLoad(() => get<Node[]>('/nodes'))
  const inbounds = useLoad(() => get<Inbound[]>('/inbounds'))
  const [nodeId, setNodeId] = useState('')
  const [tag, setTag] = useState('source-' + node.id.slice(0, 8))
  const [inboundId, setInboundId] = useState('')
  const { busy, run } = useAction()
  return <Modal title={t('引用节点 · {name}', { name: node.name })} onClose={onClose}>
    <ErrorNote error={nodes.error || inbounds.error} />
    <div className="note">{t('留空入站时创建服务器出站；选择入站时同时绑定为协议中转，沿用该入站的用户授权和流量计费。')}</div>
    <Field label={t('服务器')} htmlFor="source-ref-node"><select id="source-ref-node" className="input" value={nodeId} onChange={(e) => { setNodeId(e.target.value); setInboundId('') }}><option value="">{t('选择节点')}</option>{(nodes.data ?? []).map((n) => <option key={n.id} value={n.id}>{n.name}</option>)}</select></Field>
    <Field label={t('出站标签')} htmlFor="source-ref-tag"><input id="source-ref-tag" className="input mono" value={tag} onChange={(e) => setTag(e.target.value)} /></Field>
    <Field label={t('中转入站（可选）')} htmlFor="source-ref-inbound"><select id="source-ref-inbound" className="input" value={inboundId} onChange={(e) => setInboundId(e.target.value)}><option value="">{t('仅创建出站')}</option>{(inbounds.data ?? []).filter((i) => i.nodeId === nodeId && !i.outboundTag).map((i) => <option key={i.id} value={i.id}>{i.tag} · {i.protocol.type}</option>)}</select></Field>
    <div className="row" style={{ justifyContent: 'flex-end' }}><button className="btn" onClick={onClose}>{t('取消')}</button><button className="btn pri" disabled={busy || !nodeId || !tag.trim()} onClick={() => run(async () => { await post(`/sub-source-nodes/${node.id}/references`, { nodeId, tag, inboundId }); onSaved() }, t('已保存并下发'))}>{t('创建引用')}</button></div>
  </Modal>
}
