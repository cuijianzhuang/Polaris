// VersionPicker chooses a core version: the recent official releases as
// chips (latest / current / installed marked) plus free input.
import { useEffect, useState } from 'react'
import { get } from '../api'
import { Field } from './ui'

const vparts = (v: string) => v.replace(/^v/, '').split('.').map((x) => parseInt(x, 10) || 0)
const atLeast = (a: string, b: string) => {
  const pa = vparts(a), pb = vparts(b)
  for (let i = 0; i < 3; i++) if ((pa[i] ?? 0) !== (pb[i] ?? 0)) return (pa[i] ?? 0) > (pb[i] ?? 0)
  return true
}

export default function VersionPicker({ type, value, onChange, current, installed = [], hint }: {
  type: string; value: string; onChange: (v: string) => void; current?: string | string[]; installed?: string[]; hint?: string
}) {
  const [available, setAvailable] = useState<string[] | null>(null)
  useEffect(() => {
    let live = true
    setAvailable(null)
    get<{ versions: string[] }>('/core-versions?type=' + type)
      .then((r) => { if (live) setAvailable(r.versions ?? []) })
      .catch(() => { if (live) setAvailable([]) })
    return () => { live = false }
  }, [type])
  const cur = Array.isArray(current) ? current : current ? [current] : []
  const v = value.replace(/^v/, '')
  return (
    <>
      <Field label="版本" htmlFor="vp-ver" hint={hint ?? '从官方 GitHub Release 下载，并校验 SHA256；也可以手动填写'}>
        <input id="vp-ver" className="input mono" value={value} onChange={(e) => onChange(e.target.value.replace(/^v/, ''))} placeholder="选择下方版本或手动填写" />
      </Field>
      <div className="chips" style={{ marginBottom: 4 }} aria-label="可选版本">
        {available === null && <span className="sub">正在获取官方版本列表…</span>}
        {available?.length === 0 && <span className="sub">无法获取版本列表（可在设置中配置 GitHub 镜像），请手动填写版本号</span>}
        {available?.slice(0, 10).map((x, i) => (
          <button key={x} type="button" className={'chip' + (v === x ? ' on' : '')} onClick={() => onChange(x)}>
            v{x}{i === 0 ? ' · 最新' : ''}{cur.includes(x) ? ' · 当前' : installed.includes(x) ? ' · 已安装' : ''}
          </button>
        ))}
      </div>
      {type === 'xray' && v && atLeast(v, '26.9.8') && (
        <div className="note warn">Xray 26.9.8 起，REALITY 只接受带 X25519MLKEM768 的客户端：Shadowrocket、Clash / mihomo、sing-box 等客户端连不上 REALITY 节点（v2rayN、v2rayNG 等 Xray 内核客户端正常）。需要兼容这些客户端请选 26.7.28 或更早版本。</div>
      )}
    </>
  )
}
