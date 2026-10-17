import { useState } from 'react'
import { get, post, put } from '../api'
import { Drawer, Empty, Field, Modal, Switch, useAction, useLoad } from '../components/ui'
import { bytes, flag } from '../format'

interface CostItem {
  nodeId: string; name: string; flag: string; provider: string; amount: number; currency: string; cycle: string
  expireAt?: string; autoRenew: boolean; note: string; monthly: number; converted: boolean; daysLeft?: number; traffic30: number; perGb: number
}
interface CostSettings { base: string; rates: Record<string, number>; remindDays: number }
interface CostData { settings: CostSettings; items: CostItem[]; total: { monthly: number; yearly: number; dueSoon: number; unconverted: string[] } }

const cycles: [string, string][] = [['month', '月付'], ['quarter', '季付'], ['halfyear', '半年付'], ['year', '年付'], ['once', '一次性']]
const cycleLabel = Object.fromEntries(cycles)
const money = (v: number, cur: string) => `${v.toLocaleString(undefined, { maximumFractionDigits: 2 })} ${cur}`

function CostDrawer({ it, base, currencies, onClose, onSaved }: { it: CostItem; base: string; currencies: string[]; onClose: () => void; onSaved: () => void }) {
  const [f, setF] = useState({ provider: it.provider, amount: it.amount || '', currency: it.currency || base, cycle: it.cycle || 'month',
    expireAt: it.expireAt ? it.expireAt.slice(0, 10) : '', autoRenew: it.autoRenew, note: it.note })
  const { busy, run } = useAction()
  const set = (k: keyof typeof f, v: unknown) => setF({ ...f, [k]: v })
  return (
    <Drawer title={`服务器成本 · ${it.name}`} onClose={onClose} footer={<>
      <button className="btn" type="button" onClick={onClose}>取消</button>
      <button className="btn pri" type="button" disabled={busy} onClick={() => run(async () => {
        await put(`/nodes/${it.nodeId}/cost`, { ...f, amount: Number(f.amount) || 0 }); onSaved()
      }, '已保存')}>保存</button>
    </>}>
      <div className="form-grid">
        <Field label="服务商" htmlFor="c-prov"><input id="c-prov" className="input" value={f.provider} onChange={(e) => set('provider', e.target.value)} placeholder="Vultr / 搬瓦工 / 阿里云…" /></Field>
        <Field label="付费周期" htmlFor="c-cycle">
          <select id="c-cycle" className="input" value={f.cycle} onChange={(e) => set('cycle', e.target.value)}>{cycles.map(([k, l]) => <option key={k} value={k}>{l}</option>)}</select>
        </Field>
        <Field label="价格（每周期）" htmlFor="c-amt"><input id="c-amt" className="input mono" type="number" min="0" step="0.01" value={f.amount} onChange={(e) => set('amount', e.target.value)} /></Field>
        <Field label="币种" htmlFor="c-cur">
          <input id="c-cur" className="input mono" list="c-curs" value={f.currency} onChange={(e) => set('currency', e.target.value.toUpperCase())} />
          <datalist id="c-curs">{currencies.map((c) => <option key={c} value={c} />)}</datalist>
        </Field>
        <Field label="到期日" htmlFor="c-exp"><input id="c-exp" className="input mono" type="date" value={f.expireAt} onChange={(e) => set('expireAt', e.target.value)} /></Field>
        <Field label="自动续费" hint="开启后到期日过了自动顺延一个周期，不再提醒">
          <label className="row small"><Switch label="自动续费" on={f.autoRenew} onChange={(v) => set('autoRenew', v)} />{f.autoRenew ? '服务商自动扣费' : '需要手动续费'}</label>
        </Field>
      </div>
      <Field label="备注"><input className="input" value={f.note} onChange={(e) => set('note', e.target.value)} placeholder="账号、套餐、续费链接…" /></Field>
    </Drawer>
  )
}

function SettingsModal({ s, onClose, onSaved }: { s: CostSettings; onClose: () => void; onSaved: () => void }) {
  const [base, setBase] = useState(s.base)
  const [days, setDays] = useState(s.remindDays)
  const [rates, setRates] = useState(Object.entries(s.rates).filter(([k]) => k !== s.base).map(([k, v]) => `${k} ${v}`).join('\n'))
  const { busy, run } = useAction()
  return (
    <Modal title="汇率与提醒" onClose={onClose} footer={<>
      <button className="btn" type="button" onClick={onClose}>取消</button>
      <button className="btn pri" type="button" disabled={busy} onClick={() => run(async () => {
        const r: Record<string, number> = {}
        for (const line of rates.split('\n')) {
          const [k, v] = line.trim().split(/\s+/)
          if (k && v) r[k.toUpperCase()] = Number(v)
        }
        await put('/costs/settings', { base, rates: r, remindDays: Number(days) || 0 }); onSaved()
      }, '已保存')}>保存</button>
    </>}>
      <div className="form-grid">
        <Field label="本币" htmlFor="cs-base" hint="汇总都折算成这个币种"><input id="cs-base" className="input mono" value={base} onChange={(e) => setBase(e.target.value.toUpperCase())} /></Field>
        <Field label="提前提醒（天）" htmlFor="cs-days" hint="手动续费的服务器到期前提醒；0 = 不提醒"><input id="cs-days" className="input mono" type="number" min="0" max="90" value={days} onChange={(e) => setDays(Number(e.target.value))} /></Field>
      </div>
      <Field label="汇率" hint={`每行：币种 1 单位折合多少 ${base}，例如 USD 7.2`}>
        <textarea className="input mono" rows={8} value={rates} onChange={(e) => setRates(e.target.value)} />
      </Field>
    </Modal>
  )
}

export default function Costs() {
  const data = useLoad(() => get<CostData>('/costs'))
  const [edit, setEdit] = useState<CostItem | null>(null)
  const [settings, setSettings] = useState(false)
  const { busy, run } = useAction()
  const d = data.data
  const base = d?.settings.base ?? 'CNY'
  return (
    <>
      <div className="top">
        <div><h1>服务器成本</h1><div className="sub">每台节点服务器的价格、付费周期和到期日；汇总折算成 {base}，到期前 Telegram 提醒</div></div>
        <div className="row"><button className="btn" type="button" onClick={() => setSettings(true)} disabled={!d}>汇率与提醒</button></div>
      </div>
      {d && (
        <section className="grid g3" aria-label="汇总">
          <div className="card kpi"><div className="label">每月成本</div><div className="val">{money(d.total.monthly, base)}</div><div className="foot">{d.items.filter((i) => i.monthly).length} 台服务器计入</div></div>
          <div className="card kpi"><div className="label">每年成本</div><div className="val">{money(d.total.yearly, base)}</div><div className="foot">按当前价格和汇率估算</div></div>
          <div className="card kpi"><div className="label">即将到期</div><div className="val" style={d.total.dueSoon ? { color: 'var(--warn)' } : undefined}>{d.total.dueSoon}</div><div className="foot">{d.settings.remindDays} 天内需手动续费</div></div>
        </section>
      )}
      {d && d.total.unconverted.length > 0 && <div className="note">没有汇率的币种未计入汇总：{d.total.unconverted.join('、')}。在「汇率与提醒」里补上。</div>}
      <section className="card flush">
        {d?.items.length === 0 ? <Empty>还没有节点</Empty> : (
          <div className="scroll">
            <table>
              <thead><tr><th>节点</th><th>服务商</th><th>价格</th><th>折合每月</th><th>到期</th><th>近 30 天流量</th><th>每 GB</th><th></th></tr></thead>
              <tbody>
                {(d?.items ?? []).map((it) => (
                  <tr key={it.nodeId}>
                    <td style={{ fontWeight: 500 }}>{it.flag ? flag(it.flag) + ' ' : ''}{it.name}</td>
                    <td>{it.provider || <span className="mute">—</span>}{it.note && <div className="small mute">{it.note}</div>}</td>
                    <td className="mono">{it.amount ? <>{money(it.amount, it.currency)}<span className="mute"> / {cycleLabel[it.cycle] ?? it.cycle}</span></> : <span className="mute">未设置</span>}</td>
                    <td className="mono">{it.monthly ? money(it.monthly, base) : it.amount && !it.converted ? <span className="mute">无汇率</span> : '—'}</td>
                    <td>{it.expireAt ? <>
                      <span className="mono">{it.expireAt.slice(0, 10)}</span>
                      {it.autoRenew ? <span className="tag ok" style={{ marginLeft: 6 }}>自动续费</span>
                        : it.daysLeft !== undefined && <span className={'tag ' + (it.daysLeft < 0 ? 'bad' : it.daysLeft <= d!.settings.remindDays ? 'warn' : '')} style={{ marginLeft: 6 }}>{it.daysLeft < 0 ? `已过期 ${-it.daysLeft} 天` : `还剩 ${it.daysLeft} 天`}</span>}
                    </> : <span className="mute">—</span>}</td>
                    <td className="mono">{it.traffic30 ? bytes(it.traffic30) : '—'}</td>
                    <td className="mono">{it.perGb ? money(it.perGb, base) : '—'}</td>
                    <td style={{ textAlign: 'right', whiteSpace: 'nowrap' }}>
                      {it.expireAt && it.cycle !== 'once' && !it.autoRenew && <button className="link" type="button" disabled={busy} onClick={() => run(async () => { await post(`/nodes/${it.nodeId}/renew`); data.reload() }, '已续费，到期日顺延一个周期')}>已续费</button>}
                      <button className="link" type="button" style={{ marginLeft: 12 }} onClick={() => setEdit(it)}>编辑</button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </section>
      <div className="note">每 GB 成本 = 折合每月成本 ÷ 近 30 天服务器网卡流量（需要节点 Agent 已升级到会上报网卡流量的版本）。</div>
      {edit && d && <CostDrawer it={edit} base={base} currencies={Object.keys(d.settings.rates)} onClose={() => setEdit(null)} onSaved={() => { setEdit(null); data.reload() }} />}
      {settings && d && <SettingsModal s={d.settings} onClose={() => setSettings(false)} onSaved={() => { setSettings(false); data.reload() }} />}
    </>
  )
}
