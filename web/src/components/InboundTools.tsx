// Inbound tools modelled on 3x-ui: clone (to other nodes), import.
import { useState } from 'react'
import { post } from '../api'
import type { Inbound, Node } from '../types'
import { Field, Modal, useAction } from './ui'
import { t } from '../i18n'

export function CloneModal({ inbound, nodes, onClose, onDone }: { inbound: Inbound; nodes: Node[]; onClose: () => void; onDone: () => void }) {
  const [ids, setIds] = useState<string[]>([inbound.nodeId])
  const [port, setPort] = useState('')
  const [res, setRes] = useState<{ node: string; tag?: string; port?: number; error?: string }[] | null>(null)
  const { busy, run } = useAction()
  return (
    <Modal title={t('复制入站 · {tag}', { tag: inbound.tag })} onClose={onClose} footer={res ? <button className="btn pri" type="button" onClick={onDone}>{t('完成')}</button> : <>
      <button className="btn" type="button" onClick={onClose}>{t('取消')}</button>
      <button className="btn pri" type="button" disabled={busy || !ids.length} onClick={() => run(async () => {
        const r = await post<{ results: { node: string; tag?: string; port?: number; error?: string }[] }>(`/inbounds/${inbound.id}/clone`, { nodeIds: ids, port: Number(port) || 0 })
        setRes(r.results)
      })}>{t('复制到 {n} 个节点', { n: ids.length })}</button>
    </>}>
      {!res ? <>
        <div className="hint">{t('复制全部设置、密钥和分组授权；同一节点上的副本会使用新标签（-copy）和下一个空闲端口。')}</div>
        <Field label={t('目标节点')}>
          <div className="pick-list">
            {nodes.map((n) => (
              <label key={n.id} className="chk">
                <input type="checkbox" checked={ids.includes(n.id)} onChange={(e) => setIds(e.target.checked ? [...ids, n.id] : ids.filter((x) => x !== n.id))} />
                {n.name}{n.id === inbound.nodeId && <span className="small mute">{t('（当前节点）')}</span>}
              </label>
            ))}
          </div>
        </Field>
        <Field label={t('端口')} hint={t('留空则沿用 {port}，被占用时自动顺延', { port: inbound.port })}>
          <input className="input mono" inputMode="numeric" value={port} onChange={(e) => setPort(e.target.value)} placeholder={String(inbound.port)} />
        </Field>
      </> : (
        <table>
          <thead><tr><th>{t('节点')}</th><th>{t('结果')}</th></tr></thead>
          <tbody>{res.map((r, i) => (
            <tr key={i}><td>{r.node}</td><td>{r.error ? <span style={{ color: 'var(--bad)' }}>{r.error}</span> : <span className="mono">{r.tag} :{r.port}</span>}</td></tr>
          ))}</tbody>
        </table>
      )}
    </Modal>
  )
}

/** Import inbounds exported from this panel (one object or a list). */
export function ImportInboundModal({ nodes, onClose, onDone }: { nodes: Node[]; onClose: () => void; onDone: () => void }) {
  const [text, setText] = useState('')
  const [node, setNode] = useState(nodes[0]?.id ?? '')
  const [res, setRes] = useState<string[] | null>(null)
  const { busy, run } = useAction()
  let items: any[] = []
  let bad = false
  try { if (text.trim()) { const v = JSON.parse(text); items = Array.isArray(v) ? v : [v] } } catch { bad = true }
  return (
    <Modal wide title={t('导入入站')} onClose={onClose} footer={res ? <button className="btn pri" type="button" onClick={onDone}>{t('完成')}</button> : <>
      <button className="btn" type="button" onClick={onClose}>{t('取消')}</button>
      <button className="btn pri" type="button" disabled={busy || !items.length || !node} onClick={() => run(async () => {
        const out: string[] = []
        for (const it of items) {
          try {
            await post('/inbounds', { ...it, nodeId: node, groupIds: it.groupIds?.length ? it.groupIds : undefined })
            out.push(`✓ ${it.tag}`)
          } catch (e) { out.push(`✗ ${it.tag}：${e instanceof Error ? e.message : e}`) }
        }
        setRes(out)
      })}>{t('导入 {n} 个', { n: items.length || '' })}</button>
    </>}>
      {!res ? <>
        <div className="hint">{t('粘贴或选择本面板「导出」的入站 JSON（可以是一个或一组）。密钥会保留，原有客户端配置继续可用；标签或端口冲突时导入失败。')}</div>
        <Field label={t('导入到节点')}>
          <select className="input" value={node} onChange={(e) => setNode(e.target.value)}>{nodes.map((n) => <option key={n.id} value={n.id}>{n.name}</option>)}</select>
        </Field>
        <input type="file" accept="application/json,.json" onChange={async (e) => { const f = e.target.files?.[0]; if (f) setText(await f.text()) }} />
        <textarea className="input mono" style={{ minHeight: 220, fontSize: 12 }} value={text} onChange={(e) => setText(e.target.value)} />
        {bad && <div className="note bad">{t('JSON 格式不正确')}</div>}
      </> : <div className="code">{res.join('\n')}</div>}
    </Modal>
  )
}
