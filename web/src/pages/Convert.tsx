import { useEffect, useState } from 'react'
import { del, get, post, put } from '../api'
import { CopyBox, Drawer, Empty, ErrorNote, Field, Modal, QR, Switch, useAction, useConfirm, useLoad, useToast } from '../components/ui'
import { ago, bytes, datetime } from '../format'
import { t } from '../i18n'
import type { NodeRow, Operator, SubProfile, SubSource, SubTemplate, User } from '../types'

type Tab = 'profiles' | 'sources' | 'settings'
type SchemePreset = { id: string; name: string; group: string; desc: string; url?: string }
type Meta = { formats: string[]; regions: { Code: string; Name: string }[]; schemes?: SchemePreset[] }

const fmtLabel: Record<string, string> = { links: t('通用链接 (Base64)'), clash: 'Clash / mihomo', singbox: 'sing-box', surge: 'Surge', loon: 'Loon', quanx: 'Quantumult X' }
const protos = ['vless', 'vmess', 'trojan', 'shadowsocks', 'hysteria2', 'tuic', 'anytls', 'socks', 'http', 'wireguard']
const opLabel: Record<Operator['type'], string> = { filter: t('过滤'), rename: t('重命名'), sort: t('排序'), dedup: t('去重') }

export default function Convert() {
  const [tab, setTab] = useState<Tab>('profiles')
  const meta = useLoad(() => get<Meta>('/sub-meta'))
  const sources = useLoad(() => get<SubSource[]>('/sub-sources'))
  return (
    <>
      <div className="top">
        <div><h1>{t('订阅转换')}</h1><div className="sub">{t('聚合面板节点与外部机场订阅 / 手动节点，经过滤、重命名、排序、去重后，按客户端输出 Clash · sing-box · Surge · Loon · QX；也可交给第三方 subconverter 后端')}</div></div>
      </div>
      <div className="tabs" role="tablist">
        {([['profiles', t('订阅组')], ['sources', t('订阅源')], ['settings', t('转换设置')]] as [Tab, string][]).map(([k, l]) => (
          <button key={k} type="button" role="tab" aria-selected={tab === k} className={'tab' + (tab === k ? ' on' : '')} onClick={() => setTab(k)}>{l}</button>
        ))}
      </div>
      {tab === 'profiles' && <Profiles meta={meta.data} sources={sources.data ?? []} />}
      {tab === 'sources' && <Sources list={sources} />}
      {tab === 'settings' && <ConvertSettings meta={meta.data} />}
    </>
  )
}

// --- sources ---

type SrcForm = { id?: string; name: string; kind: 'url' | 'manual'; url: string; content: string; userAgent: string; group: string; enabled: boolean; refreshMin: number; remark: string }

function Sources({ list }: { list: ReturnType<typeof useLoad<SubSource[]>> }) {
  const [form, setForm] = useState<SrcForm | null>(null)
  const [nodes, setNodes] = useState<{ title: string; rows: NodeRow[] } | null>(null)
  const { busy, run } = useAction()
  const confirm = useConfirm()
  const toast = useToast()
  return (
    <>
      <div className="row"><span className="spacer" />
        <button className="btn pri" type="button" onClick={() => setForm({ name: '', kind: 'url', url: '', content: '', userAgent: '', group: '', enabled: true, refreshMin: 360, remark: '' })}>+ {t('订阅源')}</button>
      </div>
      <ErrorNote error={list.error} />
      <div className="card flush scroll">
        {list.data?.length === 0 ? <Empty>{t('添加机场订阅链接，或粘贴分享链接 / Clash YAML / sing-box JSON 作为手动节点。')}</Empty> : (
          <table>
            <thead><tr><th>{t('名称')}</th><th>{t('类型')}</th><th>{t('节点')}</th><th>{t('流量 / 到期')}</th><th>{t('更新')}</th><th>{t('启用')}</th><th></th></tr></thead>
            <tbody>
              {(list.data ?? []).map((s) => {
                const ui = s.userInfo
                return (
                  <tr key={s.id}>
                    <td style={{ fontWeight: 500 }}>{s.name}{s.group && <span className="tag" style={{ marginLeft: 6 }}>{s.group}</span>}
                      <div className="small mute mono" style={{ maxWidth: 320, overflow: 'hidden', textOverflow: 'ellipsis' }}>{s.kind === 'url' ? s.url.replace(/(token|key|sub)=[^&]+/gi, '$1=***') : t('手动节点')}</div></td>
                    <td><span className="tag">{s.kind === 'url' ? t('订阅链接') : t('手动')}</span></td>
                    <td className="mono">{s.nodeCount}</td>
                    <td className="small">{ui?.total ? `${bytes(ui.upload + ui.download)} / ${bytes(ui.total)}` : '—'}{ui?.expire ? <div className="mute">{t('{date} 到期', { date: datetime(new Date(ui.expire * 1000).toISOString()).slice(0, 10) })}</div> : null}</td>
                    <td className="small">{s.kind === 'url' ? <>{ago(s.fetchedAt)}{s.refreshMin ? ' · ' + t('每 {n} 分钟', { n: s.refreshMin }) : ' · ' + t('手动')}</> : '—'}
                      {s.lastError && <div style={{ color: 'var(--bad)', maxWidth: 240 }} title={s.lastError}>{s.lastError.slice(0, 60)}</div>}</td>
                    <td><Switch label={t('启用')} on={s.enabled} onChange={(v) => run(async () => { await put('/sub-sources/' + s.id, { ...s, enabled: v }); list.reload() })} /></td>
                    <td style={{ textAlign: 'right', whiteSpace: 'nowrap' }}>
                      <button className="link" type="button" onClick={() => run(async () => setNodes({ title: s.name, rows: await get<NodeRow[]>(`/sub-sources/${s.id}/nodes`) }))}>{t('节点')}</button>
                      {s.kind === 'url' && <button className="link" type="button" style={{ marginLeft: 12 }} disabled={busy} onClick={() => run(async () => {
                        const r = await post<{ nodeCount: number }>(`/sub-sources/${s.id}/refresh`); list.reload(); toast(t('已更新，{n} 个节点', { n: r.nodeCount }))
                      })}>{t('更新')}</button>}
                      <button className="link" type="button" style={{ marginLeft: 12 }} onClick={() => setForm({ ...s })}>{t('编辑')}</button>
                      <button className="link" type="button" style={{ color: 'var(--bad)', marginLeft: 12 }} onClick={async () => {
                        if (await confirm(t('删除订阅源 {name}？引用它的订阅组将不再包含这些节点。', { name: s.name }), { danger: true })) run(async () => { await del('/sub-sources/' + s.id); list.reload() })
                      }}>{t('删除')}</button>
                    </td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        )}
      </div>
      {form && (
        <Drawer title={form.id ? t('编辑订阅源') : t('添加订阅源')} onClose={() => setForm(null)} footer={<>
          <button className="btn" type="button" onClick={() => setForm(null)}>{t('取消')}</button>
          <button className="btn pri" type="button" disabled={busy || !form.name || (form.kind === 'url' ? !form.url : !form.content)} onClick={() => run(async () => {
            const r = await (form.id ? put('/sub-sources/' + form.id, form) : post('/sub-sources', form))
            setForm(null); list.reload()
            if (r?.warning) toast(t('已保存，但拉取失败：{error}', { error: r.warning }), true)
            else toast(t('已保存，解析到 {n} 个节点', { n: r?.source?.nodeCount ?? 0 }))
          })}>{busy ? t('拉取中…') : t('保存并解析')}</button>
        </>}>
          <div className="form-grid">
            <Field label={t('名称')}><input className="input" value={form.name} onChange={(e) => setForm({ ...form, name: e.target.value })} placeholder={t('机场 A')} /></Field>
            <Field label={t('分组标签')} hint={t('可用于重命名模板 {source}')}><input className="input" value={form.group} onChange={(e) => setForm({ ...form, group: e.target.value })} /></Field>
          </div>
          <Field label={t('类型')}>
            <div className="seg">
              <button type="button" className={'opt' + (form.kind === 'url' ? ' on' : '')} onClick={() => setForm({ ...form, kind: 'url' })}>{t('订阅链接')}<small>{t('Base64 / Clash / sing-box / SIP008，自动识别')}</small></button>
              <button type="button" className={'opt' + (form.kind === 'manual' ? ' on' : '')} onClick={() => setForm({ ...form, kind: 'manual' })}>{t('手动节点')}<small>{t('粘贴分享链接、YAML 或 JSON')}</small></button>
            </div>
          </Field>
          {form.kind === 'url' ? <>
            <Field label={t('订阅地址')} hint={t('只允许公网地址（可在转换设置中放开内网）')}><input className="input mono" value={form.url} onChange={(e) => setForm({ ...form, url: e.target.value })} placeholder="https://example.com/api/v1/client/subscribe?token=…" /></Field>
            <div className="form-grid">
              <Field label="User-Agent" hint={t('部分机场按 UA 返回不同格式')}><input className="input mono" value={form.userAgent} onChange={(e) => setForm({ ...form, userAgent: e.target.value })} placeholder="clash.meta" /></Field>
              <Field label={t('自动更新（分钟）')} hint={t('0 为仅手动更新')}><input className="input mono" inputMode="numeric" value={form.refreshMin} onChange={(e) => setForm({ ...form, refreshMin: Number(e.target.value) || 0 })} /></Field>
            </div>
          </> : (
            <Field label={t('节点内容')}><textarea className="input mono" style={{ minHeight: 240 }} spellCheck={false} value={form.content} onChange={(e) => setForm({ ...form, content: e.target.value })} placeholder={'vless://…\nhysteria2://…\nss://…'} /></Field>
          )}
          <Field label={t('备注')}><input className="input" value={form.remark} onChange={(e) => setForm({ ...form, remark: e.target.value })} /></Field>
        </Drawer>
      )}
      {nodes && <NodesModal title={`${nodes.title} · ${t('{n} 个节点', { n: nodes.rows.length })}`} rows={nodes.rows} onClose={() => setNodes(null)} />}
    </>
  )
}

function NodesModal({ title, rows, onClose }: { title: string; rows: NodeRow[]; onClose: () => void }) {
  return (
    <Modal title={title} onClose={onClose} wide>
      <div className="scroll" style={{ maxHeight: '60vh' }}>
        <table>
          <thead><tr><th>#</th><th>{t('名称')}</th><th>{t('协议')}</th><th>{t('地址')}</th><th>{t('传输')}</th><th>{t('地区')}</th><th>{t('来源')}</th></tr></thead>
          <tbody>
            {rows.map((r, i) => (
              <tr key={i}>
                <td className="mono small mute">{i + 1}</td>
                <td>{r.name}</td>
                <td><span className="tag">{r.protocol}</span></td>
                <td className="mono small">{r.server}:{r.port}</td>
                <td className="small">{[r.network, r.security].filter((x) => x && x !== 'none' && x !== 'tcp').join(' · ') || 'tcp'}</td>
                <td className="small">{r.region || '—'}</td>
                <td className="small mute">{r.source || t('面板')}</td>
              </tr>
            ))}
            {rows.length === 0 && <tr><td colSpan={7} className="mute">{t('没有节点')}</td></tr>}
          </tbody>
        </table>
      </div>
    </Modal>
  )
}

// --- operator chain editor ---

function OpChain({ ops, onChange, meta }: { ops: Operator[]; onChange: (o: Operator[]) => void; meta?: Meta }) {
  const set = (i: number, o: Operator) => onChange(ops.map((x, j) => (j === i ? o : x)))
  const move = (i: number, d: number) => {
    const j = i + d
    if (j < 0 || j >= ops.length) return
    const n = [...ops]; [n[i], n[j]] = [n[j], n[i]]; onChange(n)
  }
  const toggleIn = (arr: string[] | undefined, v: string) => (arr ?? []).includes(v) ? (arr ?? []).filter((x) => x !== v) : [...(arr ?? []), v]
  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 10 }}>
      {ops.map((o, i) => (
        <div key={i} className="card" style={{ padding: 12, display: 'flex', flexDirection: 'column', gap: 10, opacity: o.disabled ? 0.55 : 1 }}>
          <div className="row">
            <span className="mono small mute">{i + 1}</span>
            <b>{opLabel[o.type]}</b>
            <span className="spacer" />
            <Switch label={t('启用')} on={!o.disabled} onChange={(v) => set(i, { ...o, disabled: !v })} />
            <button className="btn sm" type="button" aria-label={t('上移')} disabled={i === 0} onClick={() => move(i, -1)}>↑</button>
            <button className="btn sm" type="button" aria-label={t('下移')} disabled={i === ops.length - 1} onClick={() => move(i, 1)}>↓</button>
            <button className="btn sm danger" type="button" onClick={() => onChange(ops.filter((_, j) => j !== i))}>{t('删除')}</button>
          </div>
          {o.type === 'filter' && <>
            <label className="chk"><input type="checkbox" checked={!!o.dropInfo} onChange={(e) => set(i, { ...o, dropInfo: e.target.checked })} />{t('去掉「剩余流量 / 到期时间 / 官网」等信息节点')}</label>
            <div className="form-grid">
              <Field label={t('保留名称匹配（正则）')}><input className="input mono" value={o.include ?? ''} onChange={(e) => set(i, { ...o, include: e.target.value })} placeholder={t('香港|日本|IEPL')} /></Field>
              <Field label={t('排除名称匹配（正则）')}><input className="input mono" value={o.exclude ?? ''} onChange={(e) => set(i, { ...o, exclude: e.target.value })} placeholder={t('(?i)倍率|x[3-9]|测试')} /></Field>
            </div>
            <Field label={t('只保留协议（不选为全部）')}>
              <div className="row">{protos.map((p) => <button key={p} type="button" className={'chip' + (o.protocols?.includes(p) ? ' on' : '')} onClick={() => set(i, { ...o, protocols: toggleIn(o.protocols, p) })}>{p}</button>)}</div>
            </Field>
            <Field label={t('只保留地区（不选为全部）')}>
              <div className="row">{(meta?.regions ?? []).map((r) => <button key={r.Code} type="button" className={'chip' + (o.regions?.includes(r.Code) ? ' on' : '')} onClick={() => set(i, { ...o, regions: toggleIn(o.regions, r.Code) })}>{r.Name}</button>)}</div>
            </Field>
          </>}
          {o.type === 'rename' && <>
            <div className="form-grid">
              <Field label={t('查找（正则）')}><input className="input mono" value={o.pattern ?? ''} onChange={(e) => set(i, { ...o, pattern: e.target.value })} placeholder="^\[.*?\]\s*" /></Field>
              <Field label={t('替换为')} hint={t('支持 $1')}><input className="input mono" value={o.replace ?? ''} onChange={(e) => set(i, { ...o, replace: e.target.value })} /></Field>
            </div>
            <Field label={t('或：名称模板')} hint={t('变量 {emoji} 旗帜 · {region} 地区代码 · {regionZh} 中文地区 · {name} 原名 · {protocol} · {source} 来源 · {index} 地区内序号 · {server} · {port}')}>
              <input className="input mono" value={o.template ?? ''} onChange={(e) => set(i, { ...o, template: e.target.value })} placeholder="{emoji} {regionZh} {index}" />
            </Field>
          </>}
          {o.type === 'sort' && <>
            <Field label={t('排序键（按点击顺序）')}>
              <div className="row">{[['region', t('地区')], ['protocol', t('协议')], ['name', t('名称')], ['server', t('地址')]].map(([k, l]) => {
                const idx = (o.sortBy ?? []).indexOf(k)
                return <button key={k} type="button" className={'chip' + (idx >= 0 ? ' on' : '')} onClick={() => set(i, { ...o, sortBy: toggleIn(o.sortBy, k) })}>{idx >= 0 ? `${idx + 1}. ` : ''}{l}</button>
              })}</div>
            </Field>
            <Field label={t('地区优先级')} hint={t('逗号分隔的地区代码，未列出的排在后面')}><input className="input mono" value={(o.regionOrder ?? []).join(',')} onChange={(e) => set(i, { ...o, regionOrder: e.target.value.split(/[,\s]+/).filter(Boolean).map((x) => x.toUpperCase()) })} placeholder="HK,TW,JP,SG,US" /></Field>
            <label className="chk"><input type="checkbox" checked={!!o.desc} onChange={(e) => set(i, { ...o, desc: e.target.checked })} />{t('倒序')}</label>
          </>}
          {o.type === 'dedup' && <div className="hint">{t('按「协议 + 地址 + 端口 + 凭据 + 路径」去掉重复节点；输出时重名节点自动加序号。')}</div>}
        </div>
      ))}
      <div className="row">
        {(Object.keys(opLabel) as Operator['type'][]).map((t) => (
          <button key={t} type="button" className="btn sm" onClick={() => onChange([...ops, t === 'filter' ? { type: t, dropInfo: true } : t === 'sort' ? { type: t, sortBy: ['region'] } : { type: t }])}>+ {opLabel[t]}</button>
        ))}
        {ops.length === 0 && <span className="hint">{t('没有处理步骤：节点原样输出')}</span>}
      </div>
    </div>
  )
}

// --- profiles ---

type ProfForm = Omit<SubProfile, 'id' | 'token' | 'downloads' | 'url'> & { id?: string }
const emptyProfile = (): ProfForm => ({ name: '', enabled: true, sourceIds: [], panelUserId: '', operators: [], useGlobalOps: true, templates: {}, converter: '', backend: '', backendConfig: '', scheme: '', skipCert: null, forceUdp: null, remark: '' })

function Profiles({ meta, sources }: { meta?: Meta; sources: SubSource[] }) {
  const list = useLoad(() => get<SubProfile[]>('/sub-profiles'))
  const tpls = useLoad(() => get<SubTemplate[]>('/sub-templates'))
  const users = useLoad(() => get<{ items: User[] }>('/users?limit=1000'))
  const [form, setForm] = useState<ProfForm | null>(null)
  const [share, setShare] = useState<SubProfile | null>(null)
  const [preview, setPreview] = useState<{ rows: NodeRow[]; info: any } | null>(null)
  const { busy, run } = useAction()
  const confirm = useConfirm()
  const tplFormats = [...new Set((tpls.data ?? []).map((t) => t.format))]
  return (
    <>
      <div className="row"><span className="spacer" />
        <button className="btn pri" type="button" onClick={() => setForm(emptyProfile())}>+ {t('订阅组')}</button>
      </div>
      <ErrorNote error={list.error} />
      <div className="card flush scroll">
        {list.data?.length === 0 ? <Empty>{t('订阅组把面板用户的节点与外部订阅源合并成一个订阅链接，客户端按 UA 或 ?format= 自动获得对应格式。')}</Empty> : (
          <table>
            <thead><tr><th>{t('名称')}</th><th>{t('包含')}</th><th>{t('处理')}</th><th>{t('转换')}</th><th>{t('下载次数')}</th><th>{t('启用')}</th><th></th></tr></thead>
            <tbody>
              {(list.data ?? []).map((p) => (
                <tr key={p.id}>
                  <td style={{ fontWeight: 500 }}>{p.name}{p.remark && <div className="small mute">{p.remark}</div>}</td>
                  <td className="small">
                    {p.panelUserId && <span className="tag xray">{t('面板')} · {users.data?.items.find((u) => u.id === p.panelUserId)?.username ?? t('用户')}</span>}
                    {(p.sourceIds ?? []).map((id) => <span key={id} className="tag">{sources.find((s) => s.id === id)?.name ?? t('已删除')}</span>)}
                  </td>
                  <td className="small">{p.useGlobalOps ? t('全局规则') : ''}{p.useGlobalOps && p.operators?.length ? ' + ' : ''}{p.operators?.length ? t('{n} 步', { n: p.operators.length }) : ''}{!p.useGlobalOps && !p.operators?.length && t('原样')}</td>
                  <td className="small">{p.converter === 'external' ? t('第三方后端') : p.converter === 'builtin' ? t('内置') : t('跟随全局')}
                    {p.scheme && <div className="mute">{schemeLabel(p.scheme, meta?.schemes)}</div>}</td>
                  <td className="mono">{p.downloads}</td>
                  <td><Switch label={t('启用')} on={p.enabled} onChange={(v) => run(async () => { await put('/sub-profiles/' + p.id, { ...p, enabled: v }); list.reload() })} /></td>
                  <td style={{ textAlign: 'right', whiteSpace: 'nowrap' }}>
                    <button className="link" type="button" onClick={() => setShare(p)}>{t('订阅链接')}</button>
                    <button className="link" type="button" style={{ marginLeft: 12 }} onClick={() => setForm({ ...p, templates: p.templates ?? {}, operators: p.operators ?? [], sourceIds: p.sourceIds ?? [] })}>{t('编辑')}</button>
                    <button className="link" type="button" style={{ color: 'var(--bad)', marginLeft: 12 }} onClick={async () => {
                      if (await confirm(t('删除订阅组 {name}？已分发的链接将失效。', { name: p.name }), { danger: true })) run(async () => { await del('/sub-profiles/' + p.id); list.reload() })
                    }}>{t('删除')}</button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>

      {form && (
        <Drawer wide title={form.id ? t('编辑订阅组 · {name}', { name: form.name }) : t('新建订阅组')} onClose={() => setForm(null)} footer={<>
          <button className="btn" type="button" disabled={busy} onClick={() => run(async () => {
            const r = await post<{ nodes: NodeRow[]; info: any }>('/sub-profiles/preview', form); setPreview({ rows: r.nodes, info: r.info })
          })}>{t('预览节点')}</button>
          <button className="btn" type="button" onClick={() => setForm(null)}>{t('取消')}</button>
          <button className="btn pri" type="button" disabled={busy || !form.name} onClick={() => run(async () => {
            await (form.id ? put('/sub-profiles/' + form.id, form) : post('/sub-profiles', form)); setForm(null); list.reload()
          }, t('已保存'))}>{t('保存')}</button>
        </>}>
          <div className="form-grid">
            <Field label={t('名称')}><input className="input" value={form.name} onChange={(e) => setForm({ ...form, name: e.target.value })} placeholder={t('全家桶')} /></Field>
            <Field label={t('备注')}><input className="input" value={form.remark} onChange={(e) => setForm({ ...form, remark: e.target.value })} /></Field>
          </div>
          <Field label={t('包含面板用户的节点')} hint={t('该用户有权限的全部入口（含 CDN / 中转），流量与到期信息也取自此用户')}>
            <select className="input" value={form.panelUserId} onChange={(e) => setForm({ ...form, panelUserId: e.target.value })}>
              <option value="">{t('不包含')}</option>
              {(users.data?.items ?? []).map((u) => <option key={u.id} value={u.id}>{u.username}</option>)}
            </select>
          </Field>
          <Field label={t('订阅源')} hint={t('按选择顺序合并')}>
            <div className="row">
              {sources.map((s) => {
                const i = form.sourceIds.indexOf(s.id)
                return <button key={s.id} type="button" className={'chip' + (i >= 0 ? ' on' : '')} aria-pressed={i >= 0}
                  onClick={() => setForm({ ...form, sourceIds: i >= 0 ? form.sourceIds.filter((x) => x !== s.id) : [...form.sourceIds, s.id] })}>{i >= 0 ? `${i + 1}. ` : ''}{s.name} · {s.nodeCount}</button>
              })}
              {sources.length === 0 && <span className="hint">{t('还没有订阅源，可在「订阅源」中添加')}</span>}
            </div>
          </Field>
          <div className="toggle"><span>{t('先应用全局处理规则（转换设置中配置）')}</span><Switch label={t('全局规则')} on={form.useGlobalOps} onChange={(v) => setForm({ ...form, useGlobalOps: v })} /></div>
          <Field label={t('本订阅组的处理步骤')} hint={t('按顺序执行：过滤 → 重命名 → 排序 → 去重，顺序可调')}>
            <OpChain ops={form.operators} onChange={(operators) => setForm({ ...form, operators })} meta={meta} />
          </Field>
          <Field label={t('规则模板')} hint={t('每种格式可选不同的规则模板（在「订阅模板」中维护多套）')}>
            <div className="form-grid">
              {tplFormats.map((f) => (
                <Field key={f} label={fmtLabel[f] ?? f}>
                  <select className="input" value={form.templates[f] ?? ''} onChange={(e) => setForm({ ...form, templates: { ...form.templates, [f]: e.target.value } })}>
                    <option value="">{t('默认模板')}</option>
                    {(tpls.data ?? []).filter((tp) => tp.format === f).map((tp) => <option key={tp.id} value={tp.id}>{tp.name}{tp.isDefault ? t('（默认）') : ''}</option>)}
                  </select>
                </Field>
              ))}
            </div>
          </Field>
          <Field label={t('规则与配置方案')} hint={t('决定策略组与分流规则；内置引擎与第三方后端通用')}>
            <SchemePicker value={form.scheme} onChange={(scheme) => setForm({ ...form, scheme })} presets={meta?.schemes} inherit />
          </Field>
          <div className="form-grid">
            <Field label={t('跳过证书校验')}>
              <TriState value={form.skipCert} onChange={(skipCert) => setForm({ ...form, skipCert })} />
            </Field>
            <Field label={t('强制开启 UDP')}>
              <TriState value={form.forceUdp} onChange={(forceUdp) => setForm({ ...form, forceUdp })} />
            </Field>
          </div>
          <Field label={t('转换引擎')}>
            <div className="seg c3">
              {([['', t('跟随全局')], ['builtin', t('内置引擎')], ['external', t('第三方后端')]] as [string, string][]).map(([k, l]) => (
                <button key={k} type="button" className={'opt' + (form.converter === k ? ' on' : '')} onClick={() => setForm({ ...form, converter: k })}>{l}</button>
              ))}
            </div>
          </Field>
          {form.converter === 'external' && (
            <div className="form-grid">
              <Field label={t('后端地址（留空用全局）')}><input className="input mono" value={form.backend} onChange={(e) => setForm({ ...form, backend: e.target.value })} placeholder="https://api.v1.mk" /></Field>
              <Field label={t('远程配置（留空用规则方案）')}><input className="input mono" value={form.backendConfig} onChange={(e) => setForm({ ...form, backendConfig: e.target.value })} placeholder="https://…/ACL4SSR_Online.ini" /></Field>
            </div>
          )}
        </Drawer>
      )}

      {preview && <NodesModal title={`${t('预览')} · ${t('{n} 个节点', { n: preview.rows.length })}${preview.info?.Total ? ` · ${t('流量 {used} / {total}', { used: bytes((preview.info.Upload ?? 0) + (preview.info.Download ?? 0)), total: bytes(preview.info.Total) })}` : ''}`} rows={preview.rows} onClose={() => setPreview(null)} />}
      {share && <ShareModal p={share} formats={meta?.formats ?? []} onClose={() => setShare(null)} onReset={async () => {
        if (!(await confirm(t('重置后旧链接立即失效，确定？'), { danger: true, ok: t('重置') }))) return
        run(async () => { const r = await post<{ token: string; url: string }>(`/sub-profiles/${share.id}/reset-token`); setShare({ ...share, ...r }); list.reload() }, t('已重置'))
      }} />}
    </>
  )
}

// --- rule schemes ---

function schemeLabel(id: string, presets?: SchemePreset[]) {
  if (id === 'none') return t('不使用方案')
  const p = presets?.find((x) => x.id === id)
  return p ? `${p.group} · ${p.name}` : t('自定义方案')
}

function TriState({ value, onChange }: { value: boolean | null; onChange: (v: boolean | null) => void }) {
  return (
    <div className="seg c3">
      {([[null, t('跟随全局')], [true, t('开启')], [false, t('关闭')]] as [boolean | null, string][]).map(([k, l]) => (
        <button key={String(k)} type="button" className={'opt' + (value === k ? ' on' : '')} onClick={() => onChange(k)}>{l}</button>
      ))}
    </div>
  )
}

/** Grouped preset picker plus a custom ini URL, with a check of the loaded scheme. */
function SchemePicker({ value, onChange, presets, inherit }: { value: string; onChange: (v: string) => void; presets?: SchemePreset[]; inherit?: boolean }) {
  const isURL = /^https?:\/\//.test(value)
  const [custom, setCustom] = useState(isURL)
  const [check, setCheck] = useState<{ groups: string[]; rules: number; remote: number; source?: string } | null>(null)
  const [err, setErr] = useState('')
  const [busy, setBusy] = useState(false)
  useEffect(() => { setCheck(null); setErr('') }, [value])
  const groups = [...new Set((presets ?? []).map((p) => p.group))]
  const sel = custom || isURL ? 'custom' : value
  const preset = presets?.find((p) => p.id === value)
  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 8 }}>
      <select className="input" value={sel} onChange={(e) => {
        const v = e.target.value
        if (v === 'custom') { setCustom(true); if (!isURL) onChange('') } else { setCustom(false); onChange(v) }
      }}>
        {inherit && <option value="">{t('跟随全局')}</option>}
        <option value={inherit ? 'none' : ''}>{t('不使用方案（按订阅模板）')}</option>
        {groups.map((g) => (
          <optgroup key={g} label={g}>
            {(presets ?? []).filter((p) => p.group === g).map((p) => <option key={p.id} value={p.id}>{p.name}</option>)}
          </optgroup>
        ))}
        <option value="custom">{t('自定义输入（ini URL）')}</option>
      </select>
      {(custom || isURL) && <input className="input mono" value={value} onChange={(e) => onChange(e.target.value.trim())} placeholder={t('https://raw.githubusercontent.com/…/config.ini（subconverter / ACL4SSR 格式）')} />}
      {preset && <div className="hint">{preset.desc}</div>}
      {value && value !== 'none' && (
        <div className="row" style={{ alignItems: 'flex-start' }}>
          <button className="btn" type="button" disabled={busy} onClick={async () => {
            setBusy(true); setErr('')
            try { setCheck(await get('/sub-scheme-check?id=' + encodeURIComponent(value))) } catch (e: any) { setErr(e?.message ?? String(e)) } finally { setBusy(false) }
          }}>{t('检查方案')}</button>
          {check && <div className="small" style={{ flex: 1 }}>{t('{groups} 个策略组 · {rules} 条规则（{remote} 个远程规则集）', { groups: check.groups.length, rules: check.rules, remote: check.remote })}
            <div className="mute" style={{ marginTop: 4 }}>{check.groups.join('　')}</div></div>}
          {err && <div className="small" style={{ color: 'var(--bad)', flex: 1 }}>{err}</div>}
        </div>
      )}
    </div>
  )
}

function ShareModal({ p, formats, onClose, onReset }: { p: SubProfile; formats: string[]; onClose: () => void; onReset: () => void }) {
  const [fmt, setFmt] = useState('')
  const url = fmt ? `${p.url}${p.url.includes('?') ? '&' : '?'}format=${fmt}` : p.url
  return (
    <Modal title={t('订阅链接 · {name}', { name: p.name })} onClose={onClose} footer={<button className="btn danger" type="button" onClick={onReset}>{t('重置链接')}</button>}>
      <Field label={t('格式')} hint={t('留空时按客户端 UA 自动识别')}>
        <div className="row">
          <button type="button" className={'chip' + (fmt === '' ? ' on' : '')} onClick={() => setFmt('')}>{t('自动')}</button>
          {formats.map((f) => <button key={f} type="button" className={'chip' + (fmt === f ? ' on' : '')} onClick={() => setFmt(f)}>{fmtLabel[f] ?? f}</button>)}
        </div>
      </Field>
      <CopyBox text={url} />
      <QR text={url} size={180} />
    </Modal>
  )
}

// --- global settings ---

function ConvertSettings({ meta }: { meta?: Meta }) {
  const s = useLoad(() => get<Record<string, any>>('/settings'))
  const [f, setF] = useState<Record<string, string>>({})
  const [ops, setOps] = useState<Operator[]>([])
  const { busy, run } = useAction()
  useEffect(() => {
    if (!s.data) return
    setF({
      converter_mode: s.data.converter_mode ?? 'builtin', converter_backend: s.data.converter_backend ?? '', converter_config: s.data.converter_config ?? '',
      converter_params: s.data.converter_params ?? '', converter_for_users: s.data.converter_for_users ?? 'false',
      sub_apply_ops_users: s.data.sub_apply_ops_users ?? 'false', allow_private_sources: s.data.allow_private_sources ?? 'false',
      sub_scheme: s.data.sub_scheme ?? '', sub_skip_cert: s.data.sub_skip_cert ?? 'false', sub_force_udp: s.data.sub_force_udp ?? 'false',
    })
    try { setOps(JSON.parse(s.data.sub_operators || '[]')) } catch { setOps([]) }
  }, [s.data])
  const set = (k: string, v: string) => setF((x) => ({ ...x, [k]: v }))
  const save = () => run(async () => { await put('/settings', { ...f, sub_operators: JSON.stringify(ops) }); s.reload() }, t('设置已保存'))
  return (
    <div className="grid g2">
      <section className="card" style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
        <h2>{t('默认转换引擎')}</h2>
        <div className="seg">
          <button type="button" className={'opt' + (f.converter_mode !== 'external' ? ' on' : '')} onClick={() => set('converter_mode', 'builtin')}>{t('内置引擎')}<small>{t('主控本地渲染，节点信息不出站')}</small></button>
          <button type="button" className={'opt' + (f.converter_mode === 'external' ? ' on' : '')} onClick={() => set('converter_mode', 'external')}>{t('第三方后端')}<small>{t('subconverter 兼容 API（ACL4SSR 等远程配置）')}</small></button>
        </div>
        {f.converter_mode === 'external' && <>
          <div className="note warn">{t('第三方后端通过短时效回调地址（3 分钟内最多 3 次）从主控拉取节点链接，拿不到用户的永久订阅地址；但节点信息本身会经过该后端，请使用自建或可信的服务。')}</div>
          <Field label={t('后端地址')}><input className="input mono" value={f.converter_backend ?? ''} onChange={(e) => set('converter_backend', e.target.value)} placeholder="https://sub.example.com" /></Field>
          <Field label={t('远程配置 URL')} hint={t('config 参数，留空时使用下方规则方案')}><input className="input mono" value={f.converter_config ?? ''} onChange={(e) => set('converter_config', e.target.value)} placeholder="https://raw.githubusercontent.com/ACL4SSR/ACL4SSR/master/Clash/config/ACL4SSR_Online.ini" /></Field>
          <Field label={t('附加参数')}><input className="input mono" value={f.converter_params ?? ''} onChange={(e) => set('converter_params', e.target.value)} placeholder="emoji=true&udp=true&scv=true" /></Field>
          <div className="toggle"><span>{t('普通用户订阅（/sub/token）也走第三方后端')}</span><Switch label={t('用户订阅')} on={f.converter_for_users === 'true'} onChange={(v) => set('converter_for_users', String(v))} /></div>
        </>}
        <h2 style={{ marginTop: 8 }}>{t('规则与配置方案')}</h2>
        <Field label={t('规则来源')} hint={t('用户订阅与订阅组的默认方案；订阅组可单独覆盖')}>
          <SchemePicker value={f.sub_scheme ?? ''} onChange={(v) => set('sub_scheme', v)} presets={meta?.schemes} />
        </Field>
        <div className="note">{t('方案采用 subconverter / ACL4SSR 的 ini 语法（')}<span className="mono">ruleset=</span>{t('、')}<span className="mono">custom_proxy_group=</span>{t('），内置引擎会转换为各客户端的原生写法：Clash 用 rule-providers，sing-box 用面板转换的 rule-set，Surge / Loon / QX 用远程规则；成员写')} <span className="mono">[]@regions</span> {t('可自动展开为有节点的地区分组。自定义订阅模板需包含')} <span className="mono">{'{{ .ProxyGroups }}'}</span> {t('与')} <span className="mono">{'{{ .Rules }}'}</span> {t('才会套用方案，否则启用方案时改用内置模板。')}</div>
        <div className="toggle"><span>{t('跳过证书校验（skip-cert-verify，自签证书节点使用）')}</span><Switch label={t('跳过证书校验')} on={f.sub_skip_cert === 'true'} onChange={(v) => set('sub_skip_cert', String(v))} /></div>
        <div className="toggle"><span>{t('强制开启 UDP 转发')}</span><Switch label={t('强制 UDP')} on={f.sub_force_udp === 'true'} onChange={(v) => set('sub_force_udp', String(v))} /></div>
        <h2 style={{ marginTop: 8 }}>{t('安全')}</h2>
        <div className="toggle"><span>{t('允许订阅源使用内网 / 回环地址（默认拒绝，防 SSRF）')}</span><Switch label={t('内网')} on={f.allow_private_sources === 'true'} onChange={(v) => set('allow_private_sources', String(v))} /></div>
        <div><button className="btn pri" type="button" disabled={busy} onClick={save}>{t('保存')}</button></div>
      </section>
      <section className="card" style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
        <h2>{t('全局处理规则')}</h2>
        <div className="sub" style={{ margin: 0 }}>{t('订阅组勾选「先应用全局处理规则」时执行；也可作用于普通用户订阅。')}</div>
        <div className="toggle"><span>{t('普通用户订阅也应用这些规则')}</span><Switch label={t('用户订阅')} on={f.sub_apply_ops_users === 'true'} onChange={(v) => set('sub_apply_ops_users', String(v))} /></div>
        <OpChain ops={ops} onChange={setOps} meta={meta} />
        <div><button className="btn pri" type="button" disabled={busy} onClick={save}>{t('保存')}</button></div>
      </section>
    </div>
  )
}
