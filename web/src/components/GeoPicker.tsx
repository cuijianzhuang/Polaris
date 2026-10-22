import { useMemo, useState } from 'react'
import { get } from '../api'
import { Modal, useLoad } from './ui'
import { t } from '../i18n'

// GeoPicker browses the built-in geosite / geoip categories (v2fly names,
// valid for Xray and sing-box) and returns the chosen ones.

type SiteCat = { name: string; count: number; attrs?: string[]; singBox: boolean }
type IPCat = { name: string; v4: number; v6: number; singBox: boolean }
type Entry = { type: number; value: string; attrs?: string[] }

const SHOWN = 200
const popular: Record<'geosite' | 'geoip', string[]> = {
  geosite: ['openai', 'anthropic', 'google', 'youtube', 'netflix', 'disney', 'spotify', 'tiktok', 'telegram', 'twitter', 'microsoft', 'apple', 'github', 'cn', 'geolocation-!cn', 'category-ads-all'],
  geoip: ['cn', 'private', 'hk', 'tw', 'jp', 'sg', 'us', 'telegram', 'google', 'cloudflare', 'netflix'],
}
const entryType = ['keyword', 'regexp', 'domain', 'full']

export default function GeoPicker({ kind, value, onChange, onClose }: {
  kind: 'geosite' | 'geoip'; value: string[]; onChange: (v: string[]) => void; onClose: () => void
}) {
  const [q, setQ] = useState('')
  const [sel, setSel] = useState<string[]>(value)
  const [open, setOpen] = useState<string | null>(null)
  const cats = useLoad(() => get<{ items: (SiteCat | IPCat)[] }>(`/routing/geo?type=${kind}`), [kind])
  const lookupQ = kind === 'geosite' && q.includes('.') && !q.includes(' ') ? q.trim() : ''
  const lookup = useLoad(() => (lookupQ ? get<{ lookup?: string[] }>(`/routing/geo?type=geosite&q=${encodeURIComponent(lookupQ)}`) : Promise.resolve({ lookup: undefined })), [lookupQ])
  const items = cats.data?.items ?? []
  const shown = useMemo(() => {
    const s = q.trim().toLowerCase()
    const hit = s ? items.filter((c) => c.name.includes(s)) : items
    // Exact and prefix matches first.
    return [...hit].sort((a, b) => Number(b.name === s) - Number(a.name === s) || Number(b.name.startsWith(s)) - Number(a.name.startsWith(s)) || a.name.localeCompare(b.name))
  }, [items, q])
  const toggle = (n: string) => setSel((x) => (x.includes(n) ? x.filter((y) => y !== n) : [...x, n]))
  return (
    <Modal wide title={kind === 'geosite' ? t('浏览 geosite 分类') : t('浏览 geoip 分类')} onClose={onClose} footer={<>
      <span className="small sub" style={{ marginRight: 'auto' }}>{t('已选 {n} 个', { n: sel.length })}</span>
      <button className="btn" type="button" onClick={onClose}>{t('取消')}</button>
      <button className="btn pri" type="button" onClick={() => { onChange(sel); onClose() }}>{t('使用')}</button>
    </>}>
      <div className="hint" style={{ marginBottom: 8 }}>
        {kind === 'geosite'
          ? t('内置 v2fly 分类，Xray 和 sing-box 通用。点「查看」看分类里的域名；带 @ 的属性（如 google@cn）只匹配其中一部分。输入域名可反查它属于哪些分类。')
          : t('内置 v2fly geoip 分类。sing-box 只有国家代码和 private 的规则集，其余分类只在 Xray 下生效。')}
      </div>
      <input className="input mono" autoFocus value={q} onChange={(e) => setQ(e.target.value)}
        placeholder={kind === 'geosite' ? t('搜索分类，或输入域名反查，如 chat.openai.com') : t('搜索，如 cn、jp、telegram')} aria-label={t('搜索分类')} />
      {!q.trim() && (
        <div className="row" style={{ flexWrap: 'wrap', gap: 6, marginTop: 10 }}>
          <span className="small sub">{t('常用：')}</span>
          {popular[kind].filter((n) => items.some((c) => c.name === n)).map((n) => (
            <button key={n} type="button" className={'chip' + (sel.includes(n) ? ' on' : '')} onClick={() => toggle(n)}>{n}</button>
          ))}
        </div>
      )}
      {sel.length > 0 && (
        <div className="row" style={{ flexWrap: 'wrap', gap: 6, marginTop: 10 }}>
          {sel.map((n) => <button key={n} type="button" className="chip on" onClick={() => toggle(n)} title={t('点击移除')}>{n} ×</button>)}
        </div>
      )}
      {lookupQ && (
        <div className="note" style={{ marginTop: 10 }}>
          {lookup.loading ? t('查找中…') : lookup.data?.lookup?.length
            ? <>{t('包含 {host} 的分类：', { host: lookupQ })}{lookup.data.lookup.map((n) => (
              <button key={n} type="button" className={'chip' + (sel.includes(n) ? ' on' : '')} style={{ margin: '4px 4px 0 0' }} onClick={() => toggle(n)}>{n}</button>))}</>
            : t('没有分类包含 {host}', { host: lookupQ })}
        </div>
      )}
      <div className="scroll" style={{ marginTop: 10, maxHeight: '52vh' }} aria-busy={cats.loading}>
        {cats.loading && <div className="sub">{t('加载中…')}</div>}
        {shown.slice(0, SHOWN).map((c) => {
          const site = kind === 'geosite' ? (c as SiteCat) : null
          const ip = kind === 'geoip' ? (c as IPCat) : null
          return (
            <div key={c.name} className="geo-row">
              <label className="chk" style={{ margin: 0, flex: 1, minWidth: 0 }}>
                <input type="checkbox" checked={sel.includes(c.name)} onChange={() => toggle(c.name)} />
                <span className="mono">{c.name}</span>
                <span className="sub small">{site ? t('{n} 条', { n: site.count }) : t('{v4} 段 IPv4 · {v6} 段 IPv6', { v4: ip!.v4, v6: ip!.v6 })}</span>
                {!c.singBox && <span className="tag warn" title={t('sing-box 没有这个分类的规则集')}>{t('仅 Xray')}</span>}
              </label>
              {site && <button type="button" className="btn sm link" onClick={() => setOpen(open === c.name ? null : c.name)}>{open === c.name ? t('收起') : t('查看')}</button>}
              {site?.attrs?.length ? (
                <div className="geo-attrs">{site.attrs.map((a) => {
                  const n = `${c.name}@${a}`
                  return <button key={a} type="button" className={'chip' + (sel.includes(n) ? ' on' : '')} onClick={() => toggle(n)}>@{a}</button>
                })}</div>
              ) : null}
              {open === c.name && <GeoEntries name={c.name} />}
            </div>
          )
        })}
        {shown.length > SHOWN && <div className="sub small" style={{ padding: 8 }}>{t('还有 {n} 个分类，继续输入以缩小范围', { n: shown.length - SHOWN })}</div>}
        {!cats.loading && !lookupQ && shown.length === 0 && <div className="sub" style={{ padding: 8 }}>{t('没有匹配的分类')}</div>}
      </div>
    </Modal>
  )
}

function GeoEntries({ name }: { name: string }) {
  const r = useLoad(() => get<{ entries: Entry[]; total: number }>(`/routing/geo/geosite/${encodeURIComponent(name)}`), [name])
  if (r.loading) return <div className="sub small geo-entries">{t('加载中…')}</div>
  const es = r.data?.entries ?? []
  return (
    <div className="geo-entries mono small">
      {es.slice(0, 60).map((e, i) => (
        <div key={i}><span className="sub">{entryType[e.type] ?? e.type}:</span>{e.value}{e.attrs?.length ? <span className="sub"> @{e.attrs.join(' @')}</span> : null}</div>
      ))}
      {(r.data?.total ?? 0) > 60 && <div className="sub">{t('…共 {n} 条', { n: r.data?.total ?? 0 })}</div>}
    </div>
  )
}
