import { useState } from 'react'
import { post } from '../api'
import GeoPicker from './GeoPicker'
import { Field, Modal, useAction } from './ui'
import { t } from '../i18n'

// WarpDialog registers a free Cloudflare WARP device for a node (from the
// panel) and adds it as a WireGuard outbound, optionally with a rule that
// sends chosen traffic through it.

const groups: { key: string; label: string; hint: string; geosite: string[] }[] = [
  { key: 'ai', label: t('AI 服务'), hint: 'OpenAI · Claude · Gemini · Perplexity', geosite: ['openai', 'anthropic', 'google-gemini', 'perplexity'] },
  { key: 'stream', label: t('流媒体'), hint: 'Netflix · Disney+ · HBO · Hulu · Prime Video', geosite: ['netflix', 'disney', 'hbo', 'hulu', 'primevideo'] },
  { key: 'google', label: 'Google / YouTube', hint: t('减少验证码和送中'), geosite: ['google', 'youtube'] },
  { key: 'social', label: 'TikTok · Spotify', hint: t('按地区限制的服务'), geosite: ['tiktok', 'spotify'] },
]

export default function WarpDialog({ nodeId, nodeName, taken, onClose, onDone }: {
  nodeId: string; nodeName: string; taken: string[]; onClose: () => void; onDone: () => void
}) {
  const [tag, setTag] = useState(taken.includes('warp') ? 'warp2' : 'warp')
  const [license, setLicense] = useState('')
  const [on, setOn] = useState<string[]>(['ai'])
  const [custom, setCustom] = useState<string[]>([])
  const [domains, setDomains] = useState('')
  const [pick, setPick] = useState(false)
  const { busy, run } = useAction()
  const geosite = [...new Set([...groups.filter((g) => on.includes(g.key)).flatMap((g) => g.geosite), ...custom])]
  const suffix = domains.split(/[\s,，]+/).map((d) => d.trim().replace(/^\*\./, '')).filter(Boolean)
  const submit = () => run(async () => {
    await post(`/nodes/${nodeId}/warp`, { tag: tag.trim(), license: license.trim(), rules: { geosite, domainSuffix: suffix } })
    onDone()
  }, geosite.length || suffix.length ? t('WARP 出站和分流规则已添加') : t('WARP 出站已添加'))
  return (
    <Modal title={t('添加 WARP 出站 · {name}', { name: nodeName })} onClose={onClose} footer={<>
      <button className="btn" type="button" onClick={onClose}>{t('取消')}</button>
      <button className="btn pri" type="button" disabled={busy || !tag.trim() || taken.includes(tag.trim())} onClick={submit}>{busy ? t('注册中…') : t('注册并添加')}</button>
    </>}>
      <div className="hint" style={{ margin: 0 }}>{t('面板向 Cloudflare 注册一个免费 WARP 设备（私钥在面板生成，不会发给 Cloudflare），自动生成 WireGuard 出站。选中的流量会经 WARP 访问，出口变成 Cloudflare 的 IP，可用于解锁 AI、流媒体等对机房 IP 有限制的服务。')}</div>
      <div className="form-grid">
        <Field label={t('出站标签')} htmlFor="warp-tag" hint={taken.includes(tag.trim()) ? t('这个标签已被占用') : undefined}>
          <input id="warp-tag" className="input mono" value={tag} onChange={(e) => setTag(e.target.value)} />
        </Field>
        <Field label={t('WARP+ 许可证（可选）')} htmlFor="warp-lic" hint={t('留空使用免费 WARP')}>
          <input id="warp-lic" className="input mono" type="password" autoComplete="off" value={license} onChange={(e) => setLicense(e.target.value)} placeholder="xxxxxxxx-xxxxxxxx-xxxxxxxx" />
        </Field>
      </div>
      <Field label={t('哪些流量走 WARP')} hint={t('都不选就只添加出站，之后可在路由规则里自己指定。')}>
        <div style={{ display: 'grid', gap: 6 }}>
          {groups.map((g) => (
            <label key={g.key} className="chk" style={{ margin: 0 }}>
              <input type="checkbox" checked={on.includes(g.key)} onChange={() => setOn((x) => (x.includes(g.key) ? x.filter((y) => y !== g.key) : [...x, g.key]))} />
              <span>{g.label}</span><span className="sub small">{g.hint}</span>
            </label>
          ))}
          <div className="row" style={{ flexWrap: 'wrap', gap: 6 }}>
            <button className="btn sm" type="button" onClick={() => setPick(true)}>{t('选择其他 geosite 分类')}</button>
            {custom.map((n) => <button key={n} type="button" className="chip on" onClick={() => setCustom((x) => x.filter((y) => y !== n))}>{n} ×</button>)}
          </div>
        </div>
      </Field>
      <Field label={t('额外的域名（可选）')} hint={t('逗号或空格分隔，含子域名，例如 example.com')}>
        <input className="input mono" value={domains} onChange={(e) => setDomains(e.target.value)} placeholder="ip.sb, example.com" />
      </Field>
      {(geosite.length > 0 || suffix.length > 0) && (
        <div className="small sub">{t('将添加规则：{rule} → {tag}', { rule: [...geosite.map((g) => 'geosite:' + g), ...suffix].join(', '), tag: tag.trim() || 'warp' })}</div>
      )}
      {pick && <GeoPicker kind="geosite" value={custom} onChange={setCustom} onClose={() => setPick(false)} />}
    </Modal>
  )
}
