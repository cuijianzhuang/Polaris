import { useSettingsDraft } from '../../components/SettingsDrafts'
import { useEffect, useState } from 'react'
import { get, post } from '../../api'
import { Field, useAction, useConfirm, useLoad } from '../../components/ui'
import { t } from '../../i18n'

export type AccessInfo = { adminPath: string; publicUrl: string; listen: string; domain?: string; subDomain?: string; updater: boolean; pending?: boolean
  last?: { action: string; value: string; ok: boolean; log: string; at: string } }

export const accessLabel: Record<string, string> = { 'set-path': t('后台路径'), 'set-domain': t('面板域名'), 'set-port': t('面板端口'), 'set-url': t('公开地址'), 'set-sub-domain': t('订阅域名') }

// The last access change is reported for a while: success for 10 minutes,
// failure for a day, so the note does not linger once it has been seen.
export const recent = (l: { ok: boolean; at: string }) => {
  const at = Date.parse(l.at)
  return !Number.isFinite(at) || Date.now() - at < (l.ok ? 10 * 60e3 : 24 * 3600e3)
}

// Access: how the panel is reached (admin path, HTTPS domain, port, public
// address). These live in /etc/polaris/polaris.env; the change is carried
// out by the root side of the server (`polaris set-path` …), which restarts
// the panel, so the page then moves to the new address.
export function Access() {
  const a = useLoad(() => get<AccessInfo>('/system/access'))
  const [f, setF] = useState<{ path?: string; domain?: string; port?: string; url?: string; sub?: string }>({})
  const [moving, setMoving] = useState<{ to: string; note: string } | null>(null)
  const { busy, run } = useAction()
  const confirm = useConfirm()
  const d = a.data
  const port = d?.listen.split(':').pop() ?? ''
  const base = (d?.publicUrl || location.origin).replace(/\/$/, '')
  const adminPath = d?.adminPath === '/' ? '' : d?.adminPath ?? ''
  const here = base + adminPath + '/'

  // After the restart: same-origin targets are polled, others opened after a pause.
  useEffect(() => {
    if (!moving) return
    let live = true
    const sameOrigin = new URL(moving.to).origin === location.origin
    const t0 = Date.now()
    const tick = async () => {
      if (!live) return
      if (sameOrigin) {
        try {
          const r = await fetch(moving.to + 'api/v1/info', { cache: 'no-store' })
          if (r.ok && Date.now() - t0 > 3000) { location.href = moving.to; return }
        } catch { /* restarting */ }
      } else if (Date.now() - t0 > 15000) { location.href = moving.to; return }
      setTimeout(tick, 1500)
    }
    setTimeout(tick, 2000)
    return () => { live = false }
  }, [moving])

  const apply = async (action: string, value: string, to: string, note: string, warn: string) => {
    if (!await confirm(`${warn}\n\n${t('应用后面板会重启，页面将跳转到：')}\n${to}`, { ok: t('应用') })) return
    const ok = await run(async () => { await post('/system/access', { action, value }); return true })
    if (ok) setMoving({ to, note })
  }

  const accessOriginal: Record<string, string> = { path: d?.adminPath ?? '', domain: d?.domain ?? '', sub: d?.subDomain ?? '', port, url: d?.publicUrl ?? '' }
  const accessDirty = !moving && Object.entries(f).some(([key, value]) => value !== undefined && value !== accessOriginal[key])
  useSettingsDraft({ label: t('面板访问'), dirty: accessDirty, busy, discard: () => setF({}) })

  if (moving) return (
    <section className="card" style={{ display: 'flex', flexDirection: 'column', gap: 10 }}>
      <h2><span className="spinner" style={{ marginRight: 8 }} />{t('正在应用…')}</h2>
      <div className="sub" style={{ margin: 0 }}>{moving.note}</div>
      <div>{t('新地址：')}<a className="mono" href={moving.to}>{moving.to}</a></div>
      <div className="hint">{t('页面会自动跳转；如果一直打不开，等一会儿手动打开上面的地址，或在服务器上执行')} <code>polaris info</code> {t('查看。')}</div>
    </section>
  )
  const newPath = (f.path ?? d?.adminPath ?? '').trim()
  const normPath = '/' + newPath.replace(/^\/+|\/+$/g, '')
  return (
    <section className="card" style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
      <h2>{t('面板访问')}</h2>
      <dl className="kv" style={{ margin: 0, gridTemplateColumns: '120px minmax(0,1fr)' }}>
        <dt>{t('后台地址')}</dt><dd className="mono" style={{ wordBreak: 'break-all' }}>{d ? here : '…'}</dd>
        <dt>{t('监听')}</dt><dd className="mono">{d?.listen ?? '…'}{d?.domain ? <span className="small mute">{t('（由 Caddy 提供 HTTPS）')}</span> : null}</dd>
      </dl>
      {d && !d.updater && <div className="note warn">{t('这台服务器还没有安装在线配置组件：在服务器上执行')} <code>polaris update --force</code> {t('安装（只需一次）后就能在这里修改。Docker 部署请改环境变量。')}</div>}
      {d?.pending && <div className="note">{t('上一项更改正在执行…')}</div>}
      {d?.last && recent(d.last) && <div className={'note ' + (d.last.ok ? '' : 'bad')}>{d.last.ok ? '✓' : '✗'} {accessLabel[d.last.action] ?? d.last.action}：{d.last.value}{' '}{d.last.ok ? t('已生效') : t('修改失败')}{!d.last.ok && d.last.log ? <div className="mono small" style={{ whiteSpace: 'pre-wrap', marginTop: 4 }}>{d.last.log}</div> : null}</div>}
      <div className="form-grid">
        <Field label={t('后台路径')} hint={t('留空或填 / 不使用路径（打开地址就是后台）；用路径时扫描器只能看到 404 或伪装站点')}>
          <div className="row" style={{ flexWrap: 'nowrap' }}>
            <input className="input mono" value={f.path ?? d?.adminPath ?? ''} placeholder="/" onChange={(e) => setF({ ...f, path: e.target.value })} />
            <button className="btn" type="button" disabled={busy || !d?.updater || f.path === undefined || normPath === (d?.adminPath ?? '')}
              onClick={() => apply('set-path', normPath, base + (normPath === '/' ? '' : normPath) + '/', t('后台路径修改后面板会重启。'),
                normPath === '/' ? t('关闭后台路径后，任何人打开这个地址都能看到登录页。请确认已使用强密码并开启二次验证。') : t('后台路径改为 {path}，旧的书签将失效。', { path: normPath }))}>{t('应用')}</button>
          </div>
        </Field>
        <Field label={t('面板域名（HTTPS）')} hint={<details className="field-help"><summary>{t('配置前请阅读')}</summary><div>{t('先把域名 A 记录解析到本服务器；会安装 Caddy 自动签发证书并放行 80 / 443。已有 Nginx / OpenResty / 宝塔等占用 443 时不要用这里：在它们里反代到本机面板端口，再改下方「公开地址」')}</div></details>}>
          <div className="row" style={{ flexWrap: 'nowrap' }}>
            <input className="input mono" value={f.domain ?? d?.domain ?? ''} placeholder="panel.example.com" onChange={(e) => setF({ ...f, domain: e.target.value })} />
            <button className="btn" type="button" disabled={busy || !d?.updater || !f.domain?.trim() || f.domain.trim() === d?.domain}
              onClick={() => apply('set-domain', f.domain!.trim(), 'https://' + f.domain!.trim().toLowerCase() + adminPath + '/', t('正在安装 Caddy 并签发证书，通常需要半分钟到一分钟。'),
                t('面板将改用 https://{domain} 访问，订阅链接也会变成这个域名（用户需重新导入；不想变可在订阅设置里填订阅域名）。已接入的节点不受影响。', { domain: f.domain!.trim() }))}>{t('启用 HTTPS')}</button>
          </div>
        </Field>
        <Field label={t('订阅域名（HTTPS）')} hint={<details className="field-help"><summary>{t('配置前请阅读')}</summary><div>{t('给订阅链接单独一个域名（可走 CDN）：先把域名解析到本服务器，这里会用 Caddy 签发证书，完成后订阅链接自动改用它。该域名只开放订阅和用户中心，后台地址不变。已有 Nginx / OpenResty / 宝塔占用 80、443 时，先在它们里把域名反代到本机面板端口，再点「配置」（会检测反代是否生效，不安装 Caddy）')}</div></details>}>
          <div className="row" style={{ flexWrap: 'nowrap' }}>
            <input className="input mono" value={f.sub ?? d?.subDomain ?? ''} placeholder="sub.example.com" onChange={(e) => setF({ ...f, sub: e.target.value })} />
            <button className="btn" type="button" disabled={busy || !d?.updater || !f.sub?.trim() || f.sub.trim().toLowerCase() === d?.subDomain}
              onClick={async () => {
                const dom = f.sub!.trim().toLowerCase().replace(/^https?:\/\//, '').replace(/\/.*$/, '')
                if (!await confirm(t('为 {domain} 签发证书并作为订阅域名？订阅链接会改用 https://{domain}，用户需要重新导入订阅；面板不会重启。', { domain: dom }), { ok: t('应用') })) return
                run(async () => { await post('/system/access', { action: 'set-sub-domain', value: dom }); a.reload() }, t('已提交，证书签发通常需要半分钟'))
              }}>{t('配置')}</button>
          </div>
        </Field>
        <Field label={t('面板端口')} hint={d?.domain ? t('已用域名访问时，这是 Caddy 转发的本机端口') : t('订阅链接里的端口会随之改变；云服务器需在安全组放行新端口')}>
          <div className="row" style={{ flexWrap: 'nowrap' }}>
            <input className="input mono" inputMode="numeric" value={f.port ?? port} onChange={(e) => setF({ ...f, port: e.target.value })} />
            <button className="btn" type="button" disabled={busy || !d?.updater || !f.port || f.port === port}
              onClick={() => apply('set-port', f.port!, d?.domain ? here : `${location.protocol}//${location.hostname}:${f.port}${adminPath}/`, t('端口修改后面板会重启。'),
                d?.domain ? t('Caddy 将转发到本机 {port} 端口，访问地址不变。', { port: f.port }) : t('面板改为 {port} 端口，订阅链接里的端口也会改变，用户需要重新导入。', { port: f.port }))}>{t('应用')}</button>
          </div>
        </Field>
        <Field label={t('公开地址')} hint={t('订阅和安装链接里使用的地址；自建反向代理时填写，一般不用改')}>
          <div className="row" style={{ flexWrap: 'nowrap' }}>
            <input className="input mono" value={f.url ?? d?.publicUrl ?? ''} placeholder="https://panel.example.com" onChange={(e) => setF({ ...f, url: e.target.value })} />
            <button className="btn" type="button" disabled={busy || !d?.updater || !f.url?.trim() || f.url.trim().replace(/\/$/, '') === d?.publicUrl}
              onClick={() => apply('set-url', f.url!.trim(), here, t('公开地址修改后面板会重启。'), t('订阅与安装链接将改用 {url}，用户需要重新导入订阅。', { url: f.url!.trim() }))}>{t('应用')}</button>
          </div>
        </Field>
      </div>
    </section>
  )
}
