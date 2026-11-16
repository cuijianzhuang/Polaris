import { useState } from 'react'
import { del, get, post, put } from '../api'
import { ProTag, useFeature } from '../components/License'
import { Drawer, Empty, Field, Modal, Switch, useAction, useConfirm, useLoad } from '../components/ui'
import { ago, bytes, datetime } from '../format'
import { t } from '../i18n'

interface RemoteSnapshot {
  version: string; capabilities: string[]
  users: number; onlineUsers: number; nodes: number; nodesOnline: number
  trafficUp: number; trafficDown: number
}

interface RemotePanel {
  id: string; name: string; baseUrl: string
  scopes: string[] | null; caPin: string; insecure: boolean; labels: string[] | null
  enabled: boolean; createdBy: string
  snapshot: RemoteSnapshot | null; snapshotAt?: string | null; lastOkAt?: string | null
  lastError: string; failures: number; nextTryAt?: string | null
  createdAt: string; updatedAt: string
  stale: boolean; ageSeconds: number; health: 'ok' | 'stale' | 'failed' | 'never'
}

interface PanelsList { panels: RemotePanel[]; pro: boolean; cap: number }

const healthMeta: Record<RemotePanel['health'], { cls: string; label: string }> = {
  ok: { cls: 'ok', label: t('健康正常') },
  stale: { cls: 'warn', label: t('陈旧') },
  failed: { cls: 'bad', label: t('失败') },
  never: { cls: '', label: t('未采集') },
}

function ageText(p: RemotePanel): string {
  if (!p.snapshotAt) return t('未采集')
  const s = p.ageSeconds
  if (s < 60) return t('刚刚')
  if (s < 3600) return t('{n} 分钟前', { n: Math.floor(s / 60) })
  if (s < 86400) return t('{n} 小时前', { n: Math.floor(s / 3600) })
  return t('{n} 天前', { n: Math.floor(s / 86400) })
}

function splitList(v: string): string[] {
  return [...new Set(v.split(/[,，]/).map((s) => s.trim()).filter(Boolean))]
}

type ActionKind = 'node.sync' | 'node.upgrade-agent' | 'user.renew' | 'user.reset-traffic' | 'user.add-traffic'

const actionOptions: { id: ActionKind; label: string; target: 'node' | 'user' }[] = [
  { id: 'node.sync', label: t('同步节点'), target: 'node' },
  { id: 'node.upgrade-agent', label: t('升级 Agent'), target: 'node' },
  { id: 'user.renew', label: t('用户续费'), target: 'user' },
  { id: 'user.reset-traffic', label: t('清零流量'), target: 'user' },
  { id: 'user.add-traffic', label: t('追加流量'), target: 'user' },
]

function ActionForm({ panel, canWrite }: { panel: RemotePanel; canWrite: boolean }) {
  const [action, setAction] = useState<ActionKind>('node.sync')
  const [targetId, setTargetId] = useState('')
  const [days, setDays] = useState(30)
  const [gb, setGb] = useState(10)
  const [resetTraffic, setResetTraffic] = useState(false)
  const { busy, run } = useAction()
  const opt = actionOptions.find((o) => o.id === action)!
  const body: Record<string, unknown> = { action }
  if (opt.target === 'node') body.nodeId = targetId
  else body.userId = targetId
  if (action === 'user.renew') { body.days = days; body.resetTraffic = resetTraffic }
  if (action === 'user.add-traffic') body.gb = gb
  return (
    <div>
      <div className="form-grid">
        <Field label={t('写操作')} htmlFor="pa-action">
          <select id="pa-action" className="input" value={action} disabled={!canWrite} onChange={(e) => setAction(e.target.value as ActionKind)}>
            {actionOptions.map((o) => <option key={o.id} value={o.id}>{o.label}</option>)}
          </select>
        </Field>
        <Field label={opt.target === 'node' ? t('节点 ID') : t('用户 ID')} htmlFor="pa-target">
          <input id="pa-target" className="input mono" value={targetId} disabled={!canWrite} onChange={(e) => setTargetId(e.target.value)} placeholder={opt.target === 'node' ? 'n_xxx' : 'u_xxx'} />
        </Field>
        {action === 'user.renew' && (
          <Field label={t('天数')} htmlFor="pa-days"><input id="pa-days" className="input mono" type="number" min="1" value={days} disabled={!canWrite} onChange={(e) => setDays(Number(e.target.value) || 0)} /></Field>
        )}
        {action === 'user.add-traffic' && (
          <Field label={t('流量（GB）')} htmlFor="pa-gb"><input id="pa-gb" className="input mono" type="number" min="0" step="0.5" value={gb} disabled={!canWrite} onChange={(e) => setGb(Number(e.target.value) || 0)} /></Field>
        )}
      </div>
      {action === 'user.renew' && (
        <label className="row small"><Switch label={t('清零流量')} on={resetTraffic} onChange={setResetTraffic} disabled={!canWrite} />{t('清零流量')}</label>
      )}
      {!canWrite && <div className="note warn">{t('需要专业版许可证才能使用写操作')}</div>}
      <div className="row" style={{ marginTop: 8 }}>
        <button className="btn pri" type="button" disabled={busy || !canWrite || !targetId.trim()} onClick={() => run(async () => { await post(`/panels/${panel.id}/actions`, body) }, t('已执行'))}>{t('执行')}</button>
      </div>
    </div>
  )
}

function PanelDrawer({ p, canWrite, onClose, onReload }: { p: RemotePanel; canWrite: boolean; onClose: () => void; onReload: () => void }) {
  const { busy, run } = useAction()
  const snap = p.snapshot
  const meta = healthMeta[p.health]
  return (
    <Drawer title={p.name} onClose={onClose} footer={<>
      <button className="btn" type="button" onClick={onClose}>{t('取消')}</button>
      <a className="btn" href={p.baseUrl} target="_blank" rel="noreferrer">{t('打开面板')}</a>
      <button className="btn pri" type="button" disabled={busy} onClick={() => run(async () => { await post(`/panels/${p.id}/probe`); onReload() }, t('探测成功'))}>{t('探测')}</button>
    </>} wide>
      <dl className="kv">
        <dt>{t('健康')}</dt><dd><span className={'tag ' + meta.cls}>{meta.label}</span>{!p.enabled && <span className="tag" style={{ marginLeft: 6 }}>{t('已停用')}</span>}</dd>
        <dt>{t('面板地址')}</dt><dd><a className="link mono" href={p.baseUrl} target="_blank" rel="noreferrer">{p.baseUrl}</a></dd>
        <dt>{t('版本')}</dt><dd>{snap?.version || t('未知')}</dd>
        <dt>{t('快照年龄')}</dt><dd>{ageText(p)}</dd>
        <dt>{t('上次成功')}</dt><dd>{ago(p.lastOkAt)}</dd>
        <dt>{t('下次重试')}</dt><dd>{p.nextTryAt ? datetime(p.nextTryAt) : '—'}</dd>
        <dt>{t('注册人')}</dt><dd>{p.createdBy || '—'}</dd>
        <dt>{t('创建时间')}</dt><dd>{datetime(p.createdAt)}</dd>
      </dl>
      <h3 style={{ marginTop: 16 }}>{t('快照')}</h3>
      {snap ? (
        <div className="grid g3" style={{ marginTop: 8 }}>
          <div className="card kpi"><div className="label">{t('在线用户')}</div><div className="val">{snap.onlineUsers}<span className="of"> / {snap.users}</span></div></div>
          <div className="card kpi"><div className="label">{t('在线节点')}</div><div className="val">{snap.nodesOnline}<span className="of"> / {snap.nodes}</span></div></div>
          <div className="card kpi"><div className="label">{t('本月流量')}</div><div className="val">{bytes(snap.trafficUp + snap.trafficDown)}</div><div className="foot">↓ {bytes(snap.trafficDown)} · ↑ {bytes(snap.trafficUp)}</div></div>
        </div>
      ) : <div className="note">{t('未采集')}</div>}
      {snap && snap.capabilities?.length > 0 && (
        <Field label={t('能力')}><div className="row" style={{ flexWrap: 'wrap', gap: 6 }}>{snap.capabilities.map((c) => <span key={c} className="tag">{c}</span>)}</div></Field>
      )}
      {p.failures > 0 && <div className="note bad" role="alert">{t('最近错误')}: {p.lastError || t('未知')} · {t('连续失败 {n} 次', { n: p.failures })}</div>}
      <Field label={t('作用域')}>
        {(p.scopes ?? []).length > 0
          ? <div className="row" style={{ flexWrap: 'wrap', gap: 6 }}>{(p.scopes ?? []).map((s) => <span key={s} className="tag mono">{s}</span>)}</div>
          : <span className="mute">{t('未授予任何作用域')}</span>}
      </Field>
      {(p.labels ?? []).length > 0 && (
        <Field label={t('标签')}><div className="row" style={{ flexWrap: 'wrap', gap: 6 }}>{(p.labels ?? []).map((s) => <span key={s} className="tag">{s}</span>)}</div></Field>
      )}
      <h3 style={{ marginTop: 16 }}>{t('写操作')}</h3>
      <ActionForm panel={p} canWrite={canWrite} />
    </Drawer>
  )
}

interface FormState {
  name: string; baseUrl: string; token: string; scopes: string[]; caPin: string
  insecure: boolean; labels: string; enabled: boolean
}

function PanelModal({ p, onClose, onSaved }: { p: RemotePanel | null; onClose: () => void; onSaved: () => void }) {
  const editing = !!p
  const [f, setF] = useState<FormState>({
    name: p?.name ?? '', baseUrl: p?.baseUrl ?? '', token: '',
    scopes: p?.scopes ?? ['panel:read'], caPin: p?.caPin ?? '',
    insecure: p?.insecure ?? false, labels: (p?.labels ?? []).join(', '), enabled: p?.enabled ?? true,
  })
  const { busy, run } = useAction()
  const set = (k: keyof FormState, v: unknown) => setF({ ...f, [k]: v })
  const save = () => run(async () => {
    const payload: Record<string, unknown> = {
      name: f.name.trim(), baseUrl: f.baseUrl.trim(),
      scopes: f.scopes, caPin: f.caPin.trim(), insecure: f.insecure, labels: splitList(f.labels),
    }
    if (!editing) {
      await post('/panels', { ...payload, token: f.token })
      onSaved()
      return
    }
    if (f.token) payload.token = f.token
    payload.enabled = f.enabled
    await put(`/panels/${p!.id}`, payload)
    onSaved()
  }, editing ? t('已保存') : t('已注册'))
  return (
    <Modal title={editing ? t('编辑面板') : t('注册面板')} onClose={onClose} footer={<>
      <button className="btn" type="button" onClick={onClose}>{t('取消')}</button>
      <button className="btn pri" type="button" disabled={busy || !f.name.trim() || !f.baseUrl.trim() || (!editing && !f.token)} onClick={save}>{t('保存')}</button>
    </>} wide>
      <div className="form-grid">
        <Field label={t('名称')} htmlFor="pm-name"><input id="pm-name" className="input" value={f.name} onChange={(e) => set('name', e.target.value)} /></Field>
        <Field label={t('面板地址')} htmlFor="pm-url"><input id="pm-url" className="input mono" value={f.baseUrl} onChange={(e) => set('baseUrl', e.target.value)} placeholder="https://panel.example.com:8443" /></Field>
        <Field label={t('令牌')} htmlFor="pm-token" hint={editing ? t('创建时必填，编辑时留空表示不更换') : t('注册后令牌只显示一次，请妥善保存')}>
          <input id="pm-token" className="input mono" type="password" autoComplete="off" value={f.token} onChange={(e) => set('token', e.target.value)} />
        </Field>
        <Field label={t('作用域')} htmlFor="pm-scopes" hint={t('由成员面板创建令牌时实际授予，这里仅记录用于展示')}>
          <div className="row" style={{ gap: 8, flexWrap: 'wrap' }}>
            <button type="button" className="chip on"
              onClick={() => set('scopes', ['panel:read', ...f.scopes.filter((s) => s !== 'panel:read')])}>
              <span className="mono">panel:read</span>
              <span style={{ opacity: 0.7 }}> · {t('读取汇总（必选）')}</span>
            </button>
            <button type="button" className={'chip' + (f.scopes.includes('panel:actions') ? ' on' : '')}
              title={t('需要专业版')}
              onClick={() => set('scopes', f.scopes.includes('panel:actions') ? f.scopes.filter((s) => s !== 'panel:actions') : [...f.scopes, 'panel:actions'])}>
              <span className="mono">panel:actions</span>
              <span style={{ opacity: 0.7 }}> · {t('写操作')}</span>
            </button>
          </div>
        </Field>
        <Field label={t('证书指纹')} htmlFor="pm-capin" hint={t('可选，SHA-256 指纹；冒号分隔或大写均可')}>
          <input id="pm-capin" className="input mono" value={f.caPin} onChange={(e) => set('caPin', e.target.value)} placeholder="ab:cd:ef:…" />
        </Field>
        <Field label={t('标签')} htmlFor="pm-labels" hint={t('多个标签用逗号分隔')}>
          <input id="pm-labels" className="input" value={f.labels} onChange={(e) => set('labels', e.target.value)} />
        </Field>
      </div>
      <Field label={t('证书校验')} hint={f.insecure ? t('已跳过校验：成员证书不会被验证，存在中间人风险') : t('校验证书；自签证书请在上方填写指纹')}>
        <label className="row small"><Switch label={t('跳过证书校验')} on={f.insecure} onChange={(v) => set('insecure', v)} />{f.insecure ? t('已跳过校验') : t('校验证书')}</label>
      </Field>
      {editing && (
        <Field label={t('启用')} hint={t('停用后不再参与汇总与轮询')}>
          <label className="row small"><Switch label={t('启用')} on={f.enabled} onChange={(v) => set('enabled', v)} />{f.enabled ? t('启用') : t('已停用')}</label>
        </Field>
      )}
      <div className="note">{t('请先在成员面板创建 API 令牌并勾选 panel:read；需要远程写操作时再勾选 panel:actions。这里的选择只记录该令牌的授权范围，不会改变成员侧权限')}</div>
    </Modal>
  )
}

export default function Panels() {
  const data = useLoad((signal) => get<PanelsList>('/panels', signal))
  const [detail, setDetail] = useState<RemotePanel | null>(null)
  const [modal, setModal] = useState<RemotePanel | null | 'new'>(null)
  const confirm = useConfirm()
  const { busy, run } = useAction()
  const canWrite = useFeature('multi_panel')
  const d = data.error ? undefined : data.data
  const panels = d?.panels ?? []
  const reload = () => { data.reload() }
  return (
    <>
      <div className="top">
        <div><h1>{t('多机')}<ProTag on={canWrite} /></h1><div className="sub">{t('在一个控制台里查看和操作多个独立的面板实例')}</div></div>
        <div className="row">
          <button className="btn" type="button" onClick={reload} disabled={data.loading}>{t('刷新')}</button>
          <button className="btn pri" type="button" onClick={() => setModal('new')}>{t('注册面板')}</button>
        </div>
      </div>
      {data.error && <div className="note bad" role="alert">{t('多机加载失败，请重试')} <button className="link" type="button" disabled={data.loading} onClick={reload}>{t('重试')}</button></div>}
      {d && !d.pro && <div className="note">{t('社区版最多登记 {n} 台面板', { n: d.cap })} · {t('共 {n} 台', { n: panels.length })}</div>}
      {d && <section className="card flush">
        {panels.length === 0 ? (
          <Empty>{t('还没有注册任何面板')}<br /><button className="link" type="button" onClick={() => setModal('new')}>{t('去注册第一台面板')}</button></Empty>
        ) : (
          <div className="scroll">
            <table className="rtable">
              <thead><tr><th>{t('名称')}</th><th>{t('版本')}</th><th>{t('健康')}</th><th>{t('用户')}</th><th>{t('节点')}</th><th>{t('本月流量')}</th><th>{t('快照年龄')}</th><th>{t('启用')}</th><th></th></tr></thead>
              <tbody>
                {panels.map((p) => {
                  const meta = healthMeta[p.health]
                  const snap = p.snapshot
                  return (
                    <tr key={p.id} onClick={() => setDetail(p)} style={{ cursor: 'pointer' }}>
                      <td style={{ fontWeight: 500 }}>{p.name}
                        {!p.enabled && <span className="tag" style={{ marginLeft: 6 }}>{t('已停用')}</span>}
                        {(p.labels ?? []).length > 0 && <div className="small mute">{(p.labels ?? []).join(', ')}</div>}
                      </td>
                      <td className="mono">{snap?.version || t('未知')}</td>
                      <td><span className={'tag ' + meta.cls}>{meta.label}</span></td>
                      <td><div className="mono">{snap ? snap.users : '—'}</div><div className="small mute">{snap ? t('在线 {n}', { n: snap.onlineUsers }) : ''}</div></td>
                      <td><div className="mono">{snap ? snap.nodes : '—'}</div><div className="small mute">{snap ? t('在线 {n}', { n: snap.nodesOnline }) : ''}</div></td>
                      <td><div className="mono">{snap ? bytes(snap.trafficUp + snap.trafficDown) : '—'}</div>{snap && <div className="small mute">↓ {bytes(snap.trafficDown)} · ↑ {bytes(snap.trafficUp)}</div>}</td>
                      <td>{ageText(p)}</td>
                      <td onClick={(e) => e.stopPropagation()}>
                        <Switch label={t('启用')} on={p.enabled} onChange={(v) => run(async () => { await put(`/panels/${p.id}`, { enabled: v }); reload() }, v ? t('已启用') : t('已停用'))} />
                      </td>
                      <td style={{ textAlign: 'right', whiteSpace: 'nowrap' }} onClick={(e) => e.stopPropagation()}>
                        <a className="link" href={p.baseUrl} target="_blank" rel="noreferrer">{t('打开面板')}</a>
                        <button className="link" type="button" style={{ marginLeft: 8 }} disabled={busy} onClick={() => run(async () => { await post(`/panels/${p.id}/probe`); reload() }, t('探测成功'))}>{t('探测')}</button>
                        <button className="link" type="button" style={{ marginLeft: 8 }} onClick={() => setModal(p)}>{t('编辑')}</button>
                        <button className="link danger" type="button" style={{ marginLeft: 8 }} onClick={async () => {
                          if (await confirm(t('确定删除这台面板吗？仅删除本机注册，不影响成员面板。'), { danger: true })) run(async () => { await del(`/panels/${p.id}`); reload() }, t('已删除'))
                        }}>{t('删除')}</button>
                      </td>
                    </tr>
                  )
                })}
              </tbody>
            </table>
          </div>
        )}
      </section>}
      {detail && <PanelDrawer p={detail} canWrite={canWrite} onClose={() => setDetail(null)} onReload={reload} />}
      {modal && <PanelModal p={modal === 'new' ? null : modal} onClose={() => setModal(null)} onSaved={() => { setModal(null); reload() }} />}
    </>
  )
}
