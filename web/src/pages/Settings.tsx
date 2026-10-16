import { useEffect, useState } from 'react'
import { get, post, put, setToken, takeOAuthResult } from '../api'
import { Icon } from '../components/Icons'
import { accents, useTheme, type ThemeMode } from '../theme'
import { CopyBox, Field, QR, useAction, useConfirm, useLoad, useToast } from '../components/ui'

// DefaultVersion picks the version new nodes install: "latest" follows the
// newest stable release, or pin one of the five most recent ones.
function DefaultVersion({ label, type, value, onChange }: { label: string; type: string; value: string; onChange: (v: string) => void }) {
  const [vers, setVers] = useState<string[] | null>(null)
  useEffect(() => {
    let live = true
    get<{ versions: string[] }>('/core-versions?type=' + type).then((r) => { if (live) setVers(r.versions ?? []) }).catch(() => { if (live) setVers([]) })
    return () => { live = false }
  }, [type])
  const cur = value.replace(/^v/, '') || 'latest'
  const recent = (vers ?? []).slice(0, 5)
  const opts = cur !== 'latest' && !recent.includes(cur) ? [...recent, cur] : recent
  return (
    <Field label={label} hint={vers === null ? '正在获取版本列表…' : vers.length === 0 ? '无法获取版本列表（可配置 GitHub 镜像）' : '「自动」时新节点安装最新稳定版'}>
      <select className="input mono" value={cur} onChange={(e) => onChange(e.target.value)}>
        <option value="latest">自动（最新{recent[0] ? `：v${recent[0]}` : ''}）</option>
        {opts.map((v, i) => <option key={v} value={v}>v{v}{i === 0 && recent[0] === v ? ' · 最新' : ''}{!recent.includes(v) ? ' · 自定义' : ''}</option>)}
      </select>
    </Field>
  )
}

// Appearance is per browser: mode plus an accent palette.
function Appearance() {
  const [t, setT] = useTheme()
  const modes: { id: ThemeMode; label: string }[] = [{ id: 'light', label: '浅色' }, { id: 'dark', label: '深色' }, { id: 'system', label: '跟随系统' }]
  return (
    <section className="card" style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
      <h2>外观</h2>
      <Field label="主题模式" hint="仅保存在当前浏览器">
        <div className="seg c3 fixed">
          {modes.map((m) => <button key={m.id} type="button" className={'opt' + (t.mode === m.id ? ' on' : '')} aria-pressed={t.mode === m.id} onClick={() => setT({ ...t, mode: m.id })}>{m.label}</button>)}
        </div>
      </Field>
      <Field label="主题色">
        <div className="swatches">
          {accents.map((a) => <button key={a.id} type="button" className={'accent-swatch' + (t.accent === a.id ? ' on' : '')} style={{ background: a.color }}
            title={a.label} aria-label={'主题色：' + a.label} aria-pressed={t.accent === a.id} onClick={() => setT({ ...t, accent: a.id })} />)}
        </div>
      </Field>
    </section>
  )
}

export default function Settings() {
  const s = useLoad(() => get<Record<string, any>>('/settings'))
  const me = useLoad(() => get<{ username: string; totpEnabled: boolean; githubLogin?: string }>('/auth/me'))
  const gh = useLoad(() => get<{ enabled: boolean; clientId: string; callbackUrl: string }>('/auth/github'))
  const [ghForm, setGhForm] = useState({ id: '', secret: '' })
  const toast = useToast()
  const confirm = useConfirm()
  useEffect(() => {
    const r = takeOAuthResult()
    if (r.bound) toast('已绑定 GitHub 账号 ' + r.bound)
    if (r.error) toast(r.error, true)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])
  useEffect(() => { if (gh.data) setGhForm((x) => ({ ...x, id: gh.data!.clientId })) }, [gh.data])
  const [f, setF] = useState<Record<string, string>>({})
  const [pw, setPw] = useState({ old: '', new: '' })
  const [totp, setTotp] = useState<{ secret: string; url: string } | null>(null)
  const [code, setCode] = useState('')
  const { busy, run } = useAction()
  useEffect(() => {
    if (s.data) setF({
      site_title: s.data.site_title ?? '', sub_update_interval: s.data.sub_update_interval ?? '12', ip_ban_minutes: s.data.ip_ban_minutes ?? '0',
      acme_email: s.data.acme_email ?? '', acme_cf_token: '', default_xray: s.data.default_xray || 'latest', default_singbox: s.data.default_singbox || 'latest',
      ip_limit_trusted: s.data.ip_limit_trusted ?? '',
    })
  }, [s.data])
  const set = (k: string, v: string) => setF((x) => ({ ...x, [k]: v }))

  return (
    <>
      <div className="top"><div><h1>设置</h1><div className="sub">面板、订阅、证书与账号安全</div></div></div>
      <div className="grid g2">
        <section className="card" style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
          <h2>常规</h2>
          <div className="form-grid">
            <Field label="站点标题" full><input className="input" value={f.site_title ?? ''} onChange={(e) => set('site_title', e.target.value)} /></Field>
            <Field label="订阅更新间隔（小时）"><input className="input mono" value={f.sub_update_interval ?? ''} onChange={(e) => set('sub_update_interval', e.target.value)} /></Field>
            <Field label="IP 超限封禁（分钟）" hint="0 为只告警不封禁"><input className="input mono" value={f.ip_ban_minutes ?? ''} onChange={(e) => set('ip_ban_minutes', e.target.value)} /></Field>
            <Field label="IP 限制受信任地址" full hint="每行一个 IP 或 CIDR，不计入用户的同时在线 IP 数（如家庭 / 公司出口、自己的中转节点）">
              <textarea className="input mono" style={{ minHeight: 56 }} value={f.ip_limit_trusted ?? ''} onChange={(e) => set('ip_limit_trusted', e.target.value)} placeholder={'203.0.113.10\n198.51.100.0/24'} />
            </Field>
            <DefaultVersion label="默认 Xray 版本" type="xray" value={f.default_xray ?? ''} onChange={(v) => set('default_xray', v)} />
            <DefaultVersion label="默认 sing-box 版本" type="singbox" value={f.default_singbox ?? ''} onChange={(v) => set('default_singbox', v)} />
          </div>
          <h2 style={{ marginTop: 8 }}>ACME（Let's Encrypt）</h2>
          <div className="hint" style={{ marginTop: -6 }}>只填邮箱即可用 HTTP 验证签发（由域名指向的节点在 80 端口应答）；再填 Cloudflare Token 则改用 DNS 验证，支持泛域名、无需 80 端口。</div>
          <div className="form-grid">
            <Field label="邮箱"><input className="input" type="email" value={f.acme_email ?? ''} onChange={(e) => set('acme_email', e.target.value)} /></Field>
            <Field label="Cloudflare DNS Token" hint={s.data?.acme_cf_token_set ? '已保存（留空不修改）' : '可选 · 需要 Zone.DNS 编辑权限'}>
              <input className="input mono" type="password" autoComplete="off" value={f.acme_cf_token ?? ''} onChange={(e) => set('acme_cf_token', e.target.value)} />
            </Field>
          </div>
          <div><button className="btn pri" type="button" disabled={busy} onClick={() => run(async () => { await put('/settings', f); s.reload() }, '设置已保存')}>保存</button></div>
        </section>
        <section className="card" style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
          <h2>部署信息</h2>
          <dl className="kv" style={{ margin: 0, gridTemplateColumns: '120px minmax(0,1fr)' }}>
            <dt>版本</dt><dd className="mono">{s.data?.version}</dd>
            <dt>后台路径</dt><dd className="mono">{s.data?.admin_path}</dd>
            <dt>公开地址</dt><dd className="mono">{s.data?.public_url || '未设置（POLARIS_PUBLIC_URL）'}</dd>
            <dt>Agent 接入点</dt><dd className="mono">{s.data?.agent_url}</dd>
          </dl>
          <Field label="CA 指纹（SHA-256）" hint="安装命令携带此指纹，Agent 首次接入时校验主控身份"><CopyBox text={s.data?.ca_sha256 ?? ''} /></Field>
          <div className="note">后台路径、监听端口、数据库与加密密钥通过环境变量或配置文件设置（见 README）。非后台路径一律返回 404 或伪装站点。</div>
        </section>
        <Appearance />
        <section className="card" style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
          <h2>修改密码 · {me.data?.username}</h2>
          <Field label="原密码"><input className="input" type="password" autoComplete="current-password" value={pw.old} onChange={(e) => setPw({ ...pw, old: e.target.value })} /></Field>
          <Field label="新密码" hint="至少 8 位"><input className="input" type="password" autoComplete="new-password" value={pw.new} onChange={(e) => setPw({ ...pw, new: e.target.value })} /></Field>
          <div><button className="btn" type="button" disabled={busy || pw.new.length < 8} onClick={() => run(async () => { const r = await post<{ token: string }>('/auth/password', pw); setToken(r.token); setPw({ old: '', new: '' }) }, '密码已修改，其他设备上的登录已退出')}>修改密码</button></div>
        </section>
        <section className="card" style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
          <h2>二次验证（TOTP）</h2>
          <div className="sub" style={{ margin: 0 }}>当前：{me.data?.totpEnabled ? '已开启' : '未开启'} · 登录失败 5 次锁定 15 分钟</div>
          {!totp ? (
            <div><button className="btn" type="button" disabled={busy} onClick={() => run(async () => setTotp(await post('/auth/totp/setup')))}>{me.data?.totpEnabled ? '重新绑定' : '开始绑定'}</button></div>
          ) : (
            <>
              <QR text={totp.url} size={180} />
              <CopyBox text={totp.secret} />
              <Field label="验证码"><input className="input mono" inputMode="numeric" value={code} onChange={(e) => setCode(e.target.value)} /></Field>
              <div className="row">
                <button className="btn pri" type="button" disabled={busy || code.length < 6} onClick={() => run(async () => { await post('/auth/totp/enable', { code, enable: true }); setTotp(null); setCode(''); me.reload() }, '二次验证已开启')}>开启</button>
              </div>
            </>
          )}
          <h2 style={{ marginTop: 8 }}><span className="row" style={{ gap: 8 }}><Icon.GitHub />GitHub 登录</span></h2>
          {me.data?.githubLogin ? (
            <div className="row">
              <span className="small">当前账号已绑定 <b>@{me.data.githubLogin}</b>，可在登录页用 GitHub 登录{me.data.totpEnabled ? '（仍需二次验证码）' : ''}</span>
              <span className="spacer" />
              <button className="btn sm danger" type="button" disabled={busy} onClick={async () => {
                if (await confirm('解除 GitHub 绑定？之后只能用密码登录。')) run(async () => { await post('/auth/github/unbind'); me.reload() }, '已解除绑定')
              }}>解除绑定</button>
            </div>
          ) : gh.data?.enabled ? (
            <div className="row">
              <span className="small mute">绑定后可在登录页一键用 GitHub 登录</span><span className="spacer" />
              <button className="btn sm" type="button" disabled={busy} onClick={() => run(async () => { location.href = (await post<{ url: string }>('/auth/github/bind-url')).url })}><Icon.GitHub />绑定 GitHub</button>
            </div>
          ) : null}
          <details className="more" open={!gh.data?.enabled}>
            <summary>OAuth App 设置{gh.data?.enabled ? '（已配置）' : ''}</summary>
            <div style={{ display: 'grid', gap: 10, marginTop: 10 }}>
              <div className="hint">在 GitHub → Settings → Developer settings → OAuth Apps 新建应用，「Authorization callback URL」填写下方地址，然后填入 Client ID 与 Client Secret。</div>
              <CopyBox text={gh.data?.callbackUrl ?? ''} />
              <div className="form-grid">
                <Field label="Client ID"><input className="input mono" value={ghForm.id} onChange={(e) => setGhForm({ ...ghForm, id: e.target.value.trim() })} /></Field>
                <Field label="Client Secret" hint={gh.data?.enabled ? '已保存（留空不修改）' : undefined}><input className="input mono" type="password" autoComplete="off" value={ghForm.secret} onChange={(e) => setGhForm({ ...ghForm, secret: e.target.value.trim() })} /></Field>
              </div>
              <div><button className="btn sm pri" type="button" disabled={busy || !ghForm.id} onClick={() => run(async () => {
                await put('/settings', ghForm.secret ? { github_client_id: ghForm.id, github_client_secret: ghForm.secret } : { github_client_id: ghForm.id })
                setGhForm((x) => ({ ...x, secret: '' })); gh.reload()
              }, 'GitHub 登录配置已保存')}>保存</button></div>
            </div>
          </details>
          {me.data?.totpEnabled && !totp && (
            <div className="row">
              <input className="input mono" style={{ width: 140 }} placeholder="验证码" value={code} onChange={(e) => setCode(e.target.value)} />
              <button className="btn danger" type="button" disabled={busy || code.length < 6} onClick={() => run(async () => { await post('/auth/totp/enable', { code, enable: false }); setCode(''); me.reload() }, '已关闭')}>关闭二次验证</button>
            </div>
          )}
        </section>
      </div>
    </>
  )
}
