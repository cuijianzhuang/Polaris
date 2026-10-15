import { useEffect, useState } from 'react'
import { get, post, put } from '../api'
import { CopyBox, Field, QR, useAction, useLoad } from '../components/ui'

export default function Settings() {
  const s = useLoad(() => get<Record<string, any>>('/settings'))
  const me = useLoad(() => get<{ username: string; totpEnabled: boolean }>('/auth/me'))
  const [f, setF] = useState<Record<string, string>>({})
  const [pw, setPw] = useState({ old: '', new: '' })
  const [totp, setTotp] = useState<{ secret: string; url: string } | null>(null)
  const [code, setCode] = useState('')
  const { busy, run } = useAction()
  useEffect(() => {
    if (s.data) setF({
      site_title: s.data.site_title ?? '', sub_update_interval: s.data.sub_update_interval ?? '12', ip_ban_minutes: s.data.ip_ban_minutes ?? '0',
      acme_email: s.data.acme_email ?? '', acme_cf_token: '', default_xray: s.data.default_xray ?? '', default_singbox: s.data.default_singbox ?? '',
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
            <Field label="默认 Xray 版本"><input className="input mono" value={f.default_xray ?? ''} onChange={(e) => set('default_xray', e.target.value)} /></Field>
            <Field label="默认 sing-box 版本"><input className="input mono" value={f.default_singbox ?? ''} onChange={(e) => set('default_singbox', e.target.value)} /></Field>
          </div>
          <h2 style={{ marginTop: 8 }}>ACME（DNS-01 · Cloudflare）</h2>
          <div className="form-grid">
            <Field label="邮箱"><input className="input" type="email" value={f.acme_email ?? ''} onChange={(e) => set('acme_email', e.target.value)} /></Field>
            <Field label="Cloudflare DNS Token" hint={s.data?.acme_cf_token_set ? '已保存（留空不修改）' : '需要 Zone.DNS 编辑权限'}>
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
        <section className="card" style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
          <h2>修改密码 · {me.data?.username}</h2>
          <Field label="原密码"><input className="input" type="password" autoComplete="current-password" value={pw.old} onChange={(e) => setPw({ ...pw, old: e.target.value })} /></Field>
          <Field label="新密码" hint="至少 8 位"><input className="input" type="password" autoComplete="new-password" value={pw.new} onChange={(e) => setPw({ ...pw, new: e.target.value })} /></Field>
          <div><button className="btn" type="button" disabled={busy || pw.new.length < 8} onClick={() => run(async () => { await post('/auth/password', pw); setPw({ old: '', new: '' }) }, '密码已修改')}>修改密码</button></div>
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
