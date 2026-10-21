import { useState } from 'react'
import { Link } from 'react-router-dom'
import NetCollectionHealth from '../components/NetCollectionHealth'
import type { NetCollectionHealth as NetHealth } from '../types'
import { receivedNetHealth } from '../netHealth'
import { get, post, put } from '../api'
import { Drawer, Empty, Field, Modal, Switch, useAction, useLoad } from '../components/ui'
import { bytes, flag } from '../format'
import { t } from '../i18n'

interface CostItem {
  netHealth?: NetHealth
  nodeId: string; name: string; flag: string; provider: string; amount: number; currency: string; cycle: string
  expireAt?: string; revision: number; autoRenew: boolean; note: string; monthly: number; converted: boolean; daysLeft?: number; traffic30: number; trafficRx30: number; trafficTx30: number; trafficMode: string; trafficDays: number; trafficLegacyDays: number; perGb: number
}
interface CostSettings { base: string; rates: Record<string, number>; remindDays: number }
interface CostData { settings: CostSettings; items: CostItem[]; total: { monthly: number; yearly: number; dueSoon: number; unconverted: string[] } }

const cycles = (): [string, string][] => [['month', t('月付')], ['quarter', t('季付')], ['halfyear', t('半年付')], ['year', t('年付')], ['once', t('一次性')]]
const trafficModeLabel = (mode: string) => ({ up: t('只算上行（出站）'), down: t('只算下行（入站）'), max: t('上下行取较大值'), sum: t('上下行合计') }[mode] ?? t('上下行合计'))
const moneyRate = (v: number, cur: string) => `${v > 0 && v < 0.000001 ? '< ' + (0.000001).toLocaleString(undefined, { maximumFractionDigits: 6 }) : v.toLocaleString(undefined, { maximumFractionDigits: 6 })} ${cur}`
const money = (v: number, cur: string) => `${v.toLocaleString(undefined, { maximumFractionDigits: 2 })} ${cur}`

const commonCurrencies = ['CNY', 'USD', 'EUR', 'GBP', 'HKD', 'JPY', 'SGD', 'TWD', 'KRW', 'RUB', 'AUD', 'CAD', 'CHF', 'USDT']
function CurrencySelect({ id, value, currencies, onChange }: {id: string; value: string; currencies: string[]; onChange: (value: string) => void}) {
  const options = [...new Set([...commonCurrencies, ...currencies, value].filter(Boolean))]
  return <select id={id} className="input mono" value={value} onChange={(e) => onChange(e.target.value)}>{options.map((cur) => <option key={cur} value={cur}>{cur}</option>)}</select>
}

function parseRates(text: string, base: string): Record<string, number> {
  const rates: Record<string, number> = {}
  for (const [i, raw] of text.split('\n').entries()) {
    if (!raw.trim()) continue
    const fields = raw.trim().split(/\s+/)
    const code = fields[0].toUpperCase()
    const rate = Number(fields[1])
    if (fields.length !== 2 || !/^[A-Z]{3,4}$/.test(code) || !Number.isFinite(rate) || rate <= 0) {
      throw new Error(t('第 {n} 行汇率无效，请填写币种代码和正数汇率', { n: i + 1 }))
    }
    if (code in rates) throw new Error(t('{currency} 的汇率重复', { currency: code }))
    if (code === base && rate !== 1) throw new Error(t('本币自身的汇率必须为 1'))
    rates[code] = rate
  }
  return { ...rates, [base]: 1 }
}

function CostDrawer({ it, base, currencies, onClose, onSaved }: { it: CostItem; base: string; currencies: string[]; onClose: () => void; onSaved: () => void }) {
  const [f, setF] = useState({ provider: it.provider, amount: it.amount || '', currency: it.currency || base, cycle: it.cycle || 'month',
    expireAt: it.expireAt ? it.expireAt.slice(0, 10) : '', autoRenew: it.autoRenew, note: it.note })
  const { busy, run } = useAction()
  const set = (k: keyof typeof f, v: unknown) => setF({ ...f, [k]: v })
  return (
    <Drawer title={t('服务器成本 · {name}', { name: it.name })} onClose={onClose} footer={<>
      <button className="btn" type="button" onClick={onClose}>{t('取消')}</button>
      <button className="btn pri" type="button" disabled={busy} onClick={() => run(async () => {
        await put(`/nodes/${it.nodeId}/cost`, { ...f, amount: Number(f.amount) || 0, expectedRevision: it.revision ?? 0 }); onSaved()
      }, t('已保存'))}>{t('保存')}</button>
    </>}>
      <div className="form-grid">
        <Field label={t('服务商')} htmlFor="c-prov"><input id="c-prov" className="input" value={f.provider} onChange={(e) => set('provider', e.target.value)} placeholder={t('Vultr / 搬瓦工 / 阿里云…')} /></Field>
        <Field label={t('付费周期')} htmlFor="c-cycle">
          <select id="c-cycle" className="input" value={f.cycle} onChange={(e) => set('cycle', e.target.value)}>{cycles().map(([k, l]) => <option key={k} value={k}>{l}</option>)}</select>
        </Field>
        <Field label={t('价格（每周期）')} htmlFor="c-amt"><input id="c-amt" className="input mono" type="number" min="0" step="0.01" value={f.amount} onChange={(e) => set('amount', e.target.value)} /></Field>
        <Field label={t('币种')} htmlFor="c-cur">
          <CurrencySelect id="c-cur" value={f.currency} currencies={[...currencies, base]} onChange={(v) => set('currency', v)} />
        </Field>
        <Field label={t('到期日')} htmlFor="c-exp"><input id="c-exp" className="input mono" type="date" value={f.expireAt} onChange={(e) => set('expireAt', e.target.value)} /></Field>
        <Field label={t('自动续费')} hint={t('开启后到期日过了自动顺延一个周期，不再提醒')}>
          <label className="row small"><Switch label={t('自动续费')} on={f.autoRenew} onChange={(v) => set('autoRenew', v)} />{f.autoRenew ? t('服务商自动扣费') : t('需要手动续费')}</label>
        </Field>
      </div>
      <Field label={t('备注')}><input className="input" value={f.note} onChange={(e) => set('note', e.target.value)} placeholder={t('账号、套餐、续费链接…')} /></Field>
    </Drawer>
  )
}

function SettingsModal({ s, onClose, onSaved }: { s: CostSettings; onClose: () => void; onSaved: () => void }) {
  const [base, setBase] = useState(s.base)
  const [days, setDays] = useState(s.remindDays)
  const [rates, setRates] = useState(Object.entries(s.rates).filter(([k]) => k !== s.base).map(([k, v]) => `${k} ${v}`).join('\n'))
  const [rateError, setRateError] = useState('')
  const currencyOptions = [...Object.keys(s.rates), ...rates.split('\n').map((line) => line.trim().split(/\s+/)[0].toUpperCase()).filter((code) => /^[A-Z]{3,4}$/.test(code))]
  const { busy, run } = useAction()
  const changeBase = (next: string) => {
    if (next === base) return
    try {
      const current = parseRates(rates, base)
      const divisor = current[next]
      if (!divisor) throw new Error(t('请先填写 {currency} 相对当前本币的汇率', { currency: next }))
      const entries = Object.entries(current).filter(([code]) => code !== next).map(([code, value]) => [code, value / divisor] as const)
      if (entries.some(([, value]) => !Number.isFinite(value) || value <= 0)) throw new Error(t('换算后的汇率超出有效范围'))
      setRates(entries.map(([code, value]) => `${code} ${Number(value.toPrecision(15))}`).join('\n'))
      setBase(next)
      setRateError('')
    } catch (e) {
      setRateError((e as Error).message)
    }
  }
  return (
    <Modal title={t('汇率与提醒')} onClose={onClose} footer={<>
      <button className="btn" type="button" onClick={onClose}>{t('取消')}</button>
      <button className="btn pri" type="button" disabled={busy} onClick={() => run(async () => {
        const parsed = parseRates(rates, base)
        await put('/costs/settings', { base, rates: parsed, remindDays: Number(days) }); onSaved()
      }, t('已保存'))}>{t('保存')}</button>
    </>}>
      <div className="form-grid">
        <Field label={t('本币')} htmlFor="cs-base" hint={t('切换本币时自动换算已填写汇率；节点原价和币种保持不变')}><CurrencySelect id="cs-base" value={base} currencies={currencyOptions} onChange={changeBase} /></Field>
        <Field label={t('提前提醒（天）')} htmlFor="cs-days" hint={t('手动续费的服务器到期前提醒；0 = 不提醒')}><input id="cs-days" className="input mono" type="number" min="0" max="90" value={days} onChange={(e) => setDays(Number(e.target.value))} /></Field>
      </div>
      {rateError && <div className="note bad" role="alert">{rateError}</div>}
      <Field label={t('汇率')} htmlFor="cs-rates" hint={t('每行填写币种代码和 1 单位折合的 {base} 金额；仅保存填写的币种', { base })}>
        <textarea id="cs-rates" className="input mono" rows={8} value={rates} onChange={(e) => { setRates(e.target.value); setRateError('') }} />
      </Field>
    </Modal>
  )
}

export default function Costs() {
  const data = useLoad(() => get<CostData>('/costs').then((d) => ({ ...d, items: d.items.map((it) => ({ ...it, netHealth: receivedNetHealth(it.netHealth) })) })))
  const [edit, setEdit] = useState<CostItem | null>(null)
  const [settings, setSettings] = useState(false)
  const { busy, run } = useAction()
  const d = data.error ? undefined : data.data
  const base = d?.settings.base ?? 'CNY'
  return (
    <>
      <div className="top">
        <div><h1>{t('服务器成本')}</h1><div className="sub">{t('每台节点服务器的价格、付费周期和到期日；汇总折算成 {base}，到期前 Telegram 提醒', { base })}</div></div>
        <div className="row"><button className="btn" type="button" onClick={data.reload} disabled={data.loading}>{t('刷新')}</button><button className="btn" type="button" onClick={() => setSettings(true)} disabled={!d}>{t('汇率与提醒')}</button></div>
      </div>
      {data.error && <div className="note bad" role="alert">{t('服务器成本加载失败，请重试')} <button className="link" type="button" disabled={data.loading} onClick={data.reload}>{t('重试')}</button></div>}
      {d && (
        <section className="grid g3" aria-label={t('汇总')}>
          <div className="card kpi"><div className="label">{t('每月成本')}</div><div className="val">{money(d.total.monthly, base)}</div><div className="foot">{t('{n} 台服务器计入', { n: d.items.filter((i) => i.monthly).length })}</div></div>
          <div className="card kpi"><div className="label">{t('每年成本')}</div><div className="val">{money(d.total.yearly, base)}</div><div className="foot">{t('按当前价格和汇率估算')}</div></div>
          <div className="card kpi"><div className="label">{t('即将到期')}</div><div className="val" style={d.total.dueSoon ? { color: 'var(--warn)' } : undefined}>{d.total.dueSoon}</div><div className="foot">{t('{n} 天内需手动续费', { n: d.settings.remindDays })}</div></div>
        </section>
      )}
      {d && d.total.unconverted.length > 0 && <div className="note">{t('没有汇率的币种未计入汇总：{list}。在「汇率与提醒」里补上。', { list: d.total.unconverted.join(', ') })}</div>}
      {d && <section className="card flush">
        {d?.items.length === 0 ? <Empty>{t('还没有节点')}</Empty> : (
          <div className="scroll">
            <table>
              <thead><tr><th>{t('节点')}</th><th>{t('服务商')}</th><th>{t('价格')}</th><th>{t('折合每月')}</th><th>{t('到期')}</th><th>{t('近 30 天流量')}</th><th>{t('每 GB')}</th><th></th></tr></thead>
              <tbody>
                {(d?.items ?? []).map((it) => (
                  <tr key={it.nodeId}>
                    <td style={{ fontWeight: 500 }}><Link className="link" to={'/nodes?id=' + encodeURIComponent(it.nodeId)} title={t('查看节点采样详情')}>{it.flag ? flag(it.flag) + ' ' : ''}{it.name}</Link></td>
                    <td>{it.provider || <span className="mute">—</span>}{it.note && <div className="small mute">{it.note}</div>}</td>
                    <td className="mono">{it.amount ? <>{money(it.amount, it.currency)}<span className="mute"> / {Object.fromEntries(cycles())[it.cycle] ?? it.cycle}</span></> : <span className="mute">{t('未设置')}</span>}</td>
                    <td className="mono">{it.monthly ? money(it.monthly, base) : it.cycle !== 'once' && it.amount && !it.converted ? <span className="mute">{t('无汇率')}</span> : '—'}</td>
                    <td>{it.expireAt ? <>
                      <span className="mono">{it.expireAt.slice(0, 10)}</span>
                      {it.autoRenew ? <span className="tag ok" style={{ marginLeft: 6 }}>{t('自动续费')}</span>
                        : it.daysLeft !== undefined && <span className={'tag ' + (it.daysLeft < 0 ? 'bad' : it.daysLeft <= d!.settings.remindDays ? 'warn' : '')} style={{ marginLeft: 6 }}>{it.daysLeft < 0 ? t('已过期 {n} 天', { n: -it.daysLeft }) : t('还剩 {n} 天', { n: it.daysLeft })}</span>}
                    </> : <span className="mute">—</span>}</td>
                    <td><div className="mono">{it.trafficDays > 0 ? bytes(it.traffic30) : '—'}</div><div className="small mute">{trafficModeLabel(it.trafficMode)}</div>{it.trafficDays > 0 && <><div className="mono small mute">↓ {bytes(it.trafficRx30)} · ↑ {bytes(it.trafficTx30)}</div><div className="small mute">{t('有数据 {n}/30 天', { n: it.trafficDays })}</div>{it.trafficLegacyDays > 0 && <div className="small" style={{ color: 'var(--warn)' }}>{t('含旧版采集记录，需核对')}</div>}</>}<NetCollectionHealth health={it.netHealth} compact /></td>
                    <td className="mono">{it.monthly > 0 && it.traffic30 > 0 ? moneyRate(it.perGb, base) : '—'}</td>
                    <td style={{ textAlign: 'right', whiteSpace: 'nowrap' }}>
                      {it.expireAt && it.cycle !== 'once' && !it.autoRenew && <button className="link" type="button" disabled={busy} onClick={() => run(async () => { await post(`/nodes/${it.nodeId}/renew`); data.reload() }, t('已续费，到期日顺延一个周期'))}>{t('已续费')}</button>}
                      <button className="link" type="button" style={{ marginLeft: 12 }} onClick={() => setEdit(it)}>{t('编辑')}</button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </section>}
      <div className="note">{t('近 30 天包含今天，按节点的计费方式统计服务器网卡流量；包含服务器其他程序的网络用量，与用户套餐流量不同。每 GB 成本 = 折合每月成本 ÷ 对应流量。')}</div>
      {edit && d && <CostDrawer it={edit} base={base} currencies={Object.keys(d.settings.rates)} onClose={() => setEdit(null)} onSaved={() => { setEdit(null); data.reload() }} />}
      {settings && d && <SettingsModal s={d.settings} onClose={() => setSettings(false)} onSaved={() => { setSettings(false); data.reload() }} />}
    </>
  )
}
