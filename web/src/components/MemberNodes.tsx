import { useMemo, useState } from 'react'
import { del, get, post, put } from '../api'
import { Drawer, Empty, Field, Modal, Switch, useAction, useConfirm, useLoad } from './ui'
import InboundFields, { type InboundDraft } from './InboundFields'
import { t } from '../i18n'
import type { CapSet, Inbound, Node } from '../types'

// Member (registered user) write UI for their own nodes — batch 2 of
// docs/design/panel-member-login.md. Everything here talks to the same
// admin routes an operator would, but the server resolves each target
// through the member ownership checks and runs the portal's flows, so
// the forms only carry the self-service fields: no groups, templates,
// CDN, relay, outbound routing or speed limits (the request bodies do
// not even contain those keys).

export type MemberInstall = { cmd: string; nodeId: string }

const splitLabels = (s: string) => s.split(/[,\s]+/).filter(Boolean)

// MemberNodeForm creates or renames one of the member's own nodes. Only
// the display fields exist here; the server fixes everything else
// (internal name, traffic rate, review flow) exactly as the portal does.
export function MemberNodeForm({ node, onClose, onSaved }: {
  node?: Node
  onClose: () => void
  onSaved: (install?: MemberInstall) => void
}) {
  const [f, setF] = useState({
    displayName: node?.displayName ?? '',
    address: node?.address ?? '',
    region: node?.region ?? '',
    flag: node?.flag ?? '',
    labels: (node?.labels ?? []).join(', '),
  })
  const { busy, run } = useAction()
  const set = (k: keyof typeof f, v: string) => setF((x) => ({ ...x, [k]: v }))
  const save = () => run(async () => {
    const body = {
      displayName: f.displayName.trim(), address: f.address.trim(), region: f.region.trim(),
      flag: f.flag.trim(), labels: splitLabels(f.labels),
    }
    if (!body.displayName) throw new Error(t('请填写节点名称'))
    if (node) {
      await put('/nodes/' + node.id, body)
      onSaved(undefined)
    } else {
      const r = await post<{ node: Node; command: string }>('/nodes', body)
      onSaved({ cmd: r.command, nodeId: r.node.id })
    }
  })
  return (
    <Modal title={node ? t('编辑节点') : t('添加节点')} onClose={onClose} footer={<>
      <button className="btn" type="button" onClick={onClose}>{t('取消')}</button>
      <button className="btn pri" type="button" disabled={busy} onClick={save}>{node ? t('保存') : t('创建并获取安装命令')}</button>
    </>}>
      <Field label={t('节点名称')} htmlFor="mn-name" hint={t('给你自己的服务器起个名字，仅用于识别')}>
        <input id="mn-name" className="input" value={f.displayName} maxLength={64} onChange={(e) => set('displayName', e.target.value)} placeholder={t('例如：香港小鸡')} />
      </Field>
      <Field label={t('地址')} htmlFor="mn-addr" hint={t('服务器的 IP 或你自己的域名')}>
        <input id="mn-addr" className="input" value={f.address} onChange={(e) => set('address', e.target.value)} placeholder="203.0.113.10" />
      </Field>
      <div className="row" style={{ gap: 12 }}>
        <Field label={t('地区')} htmlFor="mn-region">
          <input id="mn-region" className="input" value={f.region} onChange={(e) => set('region', e.target.value)} placeholder={t('例如：香港')} />
        </Field>
        <Field label={t('旗帜代码')} htmlFor="mn-flag" hint={t('两位国家/地区代码，如 HK、JP、US')}>
          <input id="mn-flag" className="input" value={f.flag} maxLength={2} onChange={(e) => set('flag', e.target.value)} placeholder="HK" />
        </Field>
      </div>
      <Field label={t('标签')} htmlFor="mn-labels" hint={t('多个标签用逗号或空格分隔')}>
        <input id="mn-labels" className="input" value={f.labels} onChange={(e) => set('labels', e.target.value)} />
      </Field>
      {!node && <div className="note">{t('创建后会生成一行安装命令，在你的服务器上执行即可接入。节点需经管理员审批后才会下发配置。')}</div>}
    </Modal>
  )
}

const emptyDraft = (): InboundDraft => ({
  core: 'xray',
  protocol: { type: 'vless', flow: 'xtls-rprx-vision' },
  transport: { type: 'tcp' },
  security: { type: 'reality', realityDest: 'www.wikipedia.org:443' },
  sniffing: { enabled: true },
})

// MemberInboundDrawer is the member's inbound editor: the shared
// InboundFields form (protocol/transport/security) with the admin-only
// blocks absent — no certificate picker (certs are not passed), no
// REALITY scanner (nodeId is not passed), no Snell owner picker — and a
// save body stripped of the fields the server would sanitise anyway
// (certificateId, fallbacks, snellUserId, acceptProxyProtocol), with no
// group/relay/template/CDN/outbound keys at all.
function MemberInboundDrawer({ node, inbound, onClose, onSaved }: {
  node: Node
  inbound?: Inbound
  onClose: () => void
  onSaved: () => void
}) {
  const caps = useLoad((signal) => get<CapSet[]>('/capabilities?node=' + node.id, signal), [node.id])
  const [draft, setDraft] = useState<InboundDraft>(() => inbound ? {
    core: inbound.core, protocol: inbound.protocol, transport: inbound.transport, security: inbound.security, sniffing: inbound.sniffing,
  } : emptyDraft())
  const [tag, setTag] = useState(inbound?.tag ?? '')
  const [port, setPort] = useState(String(inbound?.port ?? 443))
  const [listen, setListen] = useState(inbound?.listen && inbound.listen !== '::' ? inbound.listen : '')
  const [enabled, setEnabled] = useState(inbound?.enabled ?? true)
  const [remark, setRemark] = useState(inbound?.remark ?? '')
  const { busy, run } = useAction()
  const allCaps = caps.data ?? []
  const coreCaps = useMemo(() => allCaps.filter((c) => c.core === draft.core), [allCaps, draft.core])
  const coreChoices = useMemo(() => {
    const seen = new Map<string, { core: InboundDraft['core']; version?: string }>()
    for (const c of node.cores) if (!seen.has(c.type)) seen.set(c.type, { core: c.type, version: c.version })
    return [...seen.values()]
  }, [node.cores])
  const save = () => run(async () => {
    const protocol = { ...draft.protocol }
    delete protocol.fallbacks
    delete protocol.snellUserId
    const transport = { ...draft.transport }
    delete transport.acceptProxyProtocol
    const security = { ...draft.security }
    delete security.certificateId
    const body = {
      nodeId: node.id, core: draft.core, tag: tag.trim(), port: Number(port),
      listen: listen.trim() || undefined, protocol, transport, security,
      sniffing: draft.sniffing, enabled, remark: remark.trim(),
    }
    if (!body.tag) throw new Error(t('请填写入站标签'))
    if (inbound) await put('/inbounds/' + inbound.id, body)
    else await post('/inbounds', body)
    onSaved()
  })
  return (
    <Drawer title={inbound ? t('编辑入站 · {tag}', { tag: inbound.tag }) : t('添加入站')} onClose={onClose} wide footer={<>
      <button className="btn" type="button" onClick={onClose}>{t('取消')}</button>
      <button className="btn pri" type="button" disabled={busy || !tag.trim()} onClick={save}>{t('保存并下发')}</button>
    </>}>
      <div className="row" style={{ gap: 12 }}>
        <Field label={t('标签')} htmlFor="mi-tag">
          <input id="mi-tag" className="input" value={tag} onChange={(e) => setTag(e.target.value)} placeholder="vless-reality" />
        </Field>
        <Field label={t('端口')} htmlFor="mi-port">
          <input id="mi-port" className="input" inputMode="numeric" value={port} onChange={(e) => setPort(e.target.value)} />
        </Field>
        <Field label={t('监听地址')} htmlFor="mi-listen" hint={t('留空表示监听全部地址')}>
          <input id="mi-listen" className="input" value={listen} onChange={(e) => setListen(e.target.value)} placeholder="::" />
        </Field>
      </div>
      <InboundFields draft={draft} onChange={setDraft} caps={coreCaps} allCaps={allCaps} coreChoices={coreChoices} hideSnellOwner />
      <Field label={t('备注')} htmlFor="mi-remark">
        <input id="mi-remark" className="input" value={remark} onChange={(e) => setRemark(e.target.value)} />
      </Field>
      <div className="row" style={{ alignItems: 'center', gap: 8 }}>
        <Switch on={enabled} onChange={setEnabled} label={t('启用')} /><span className="small">{t('启用')}</span>
      </div>
      <div className="note">{t('入站只绑定你自己的账号：审批通过后，你可以在订阅里使用它。')}</div>
    </Drawer>
  )
}

// MemberInbounds is the inbound section on a member's node detail: the
// list of their inbounds plus create/edit/delete. Unapproved nodes show
// the review state instead — the server refuses their inbounds with a
// 403 anyway, so the entry is disabled with the reason up front.
export function MemberInbounds({ node }: { node: Node }) {
  const [editing, setEditing] = useState<Inbound | 'new' | null>(null)
  const confirm = useConfirm()
  const { run } = useAction()
  const list = useLoad((signal) => get<Inbound[]>('/inbounds?node=' + node.id, signal), [node.id])
  if (node.reviewState === 'pending') {
    return <section className="card"><div className="ctitle"><span>{t('入站')}</span></div>
      <div className="note warn">{t('节点待管理员审批，通过后才能管理入站。')}</div></section>
  }
  if (node.reviewState === 'rejected') {
    return <section className="card"><div className="ctitle"><span>{t('入站')}</span></div>
      <div className="note bad">{t('节点已被管理员拒绝，无法管理入站。请删除后重新添加，或联系管理员。')}</div></section>
  }
  const rows = list.data ?? []
  return (
    <section className="card">
      <div className="ctitle" style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
        <span>{t('入站')}</span>
        <button className="btn sm pri" type="button" onClick={() => setEditing('new')}>+ {t('添加入站')}</button>
      </div>
      {list.loading ? <div className="sub">{t('加载中…')}</div> : rows.length === 0 ? <Empty>{t('还没有入站。添加后，你的订阅即可使用这个节点。')}</Empty> : (
        <div className="scroll"><table>
          <thead><tr><th>{t('标签')}</th><th>{t('协议')}</th><th>{t('端口')}</th><th>{t('状态')}</th><th></th></tr></thead>
          <tbody>
            {rows.map((i) => (
              <tr key={i.id}>
                <td className="mono">{i.tag}</td>
                <td>{i.protocol.type} · {i.transport.type}{i.security.type !== 'none' ? ' · ' + i.security.type : ''}</td>
                <td className="mono">{i.port}</td>
                <td><span className={'st ' + (i.enabled ? 'ok' : 'off')}>{i.enabled ? t('启用') : t('停用')}</span></td>
                <td><div className="row" style={{ gap: 6, justifyContent: 'flex-end' }}>
                  <button className="btn sm" type="button" onClick={() => setEditing(i)}>{t('编辑')}</button>
                  <button className="btn sm danger" type="button" onClick={async () => {
                    if (await confirm(t('删除入站 {tag}？', { tag: i.tag }), { danger: true, ok: t('删除') }))
                      run(async () => { await del('/inbounds/' + i.id); list.reload() }, t('已删除'))
                  }}>{t('删除')}</button>
                </div></td>
              </tr>
            ))}
          </tbody>
        </table></div>
      )}
      {editing && <MemberInboundDrawer node={node} inbound={editing === 'new' ? undefined : editing}
        onClose={() => setEditing(null)} onSaved={() => { setEditing(null); list.reload() }} />}
    </section>
  )
}
