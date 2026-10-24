import type { Operator } from '../types'
import { t } from '../i18n'
import { Field, Switch } from './ui'
const protos = ['vless', 'vmess', 'trojan', 'shadowsocks', 'hysteria2', 'tuic', 'anytls', 'snell', 'socks', 'http', 'wireguard']
const opLabel: Record<Operator['type'], string> = { filter: t('过滤'), rename: t('重命名'), sort: t('排序'), dedup: t('去重') }

export default function OperatorChain({ ops, onChange, meta, maxSteps }: { ops: Operator[]; onChange: (o: Operator[]) => void; meta?: { regions: { Code: string; Name: string }[] }; maxSteps?: number }) {
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
          <button key={t} type="button" className="btn sm" disabled={maxSteps !== undefined && ops.length >= maxSteps} onClick={() => onChange([...ops, t === 'filter' ? { type: t, dropInfo: true } : t === 'sort' ? { type: t, sortBy: ['region'] } : { type: t }])}>+ {opLabel[t]}</button>
        ))}
        {ops.length === 0 && <span className="hint">{t('没有处理步骤：节点原样输出')}</span>}
      </div>
    </div>
  )
}

