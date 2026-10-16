import { useEffect, useState } from 'react'
import { del, get, post, put } from '../api'
import { ago } from '../format'
import { copyText, Field, Modal, useAction, useConfirm, useLoad } from './ui'

// RecoveryCodesModal shows freshly issued recovery codes once.
export function RecoveryCodesModal({ codes, onClose }: { codes: string[]; onClose: () => void }) {
  const text = codes.join('\n')
  const save = () => {
    const a = document.createElement('a')
    a.href = URL.createObjectURL(new Blob([`Polaris 二次验证恢复码（每个只能用一次）\n${location.host}\n\n${text}\n`], { type: 'text/plain' }))
    a.download = 'polaris-recovery-codes.txt'
    a.click()
  }
  return (
    <Modal title="保存恢复码" onClose={onClose} footer={<>
      <button className="btn" type="button" onClick={() => copyText(text)}>复制</button>
      <button className="btn" type="button" onClick={save}>下载</button>
      <button className="btn pri" type="button" onClick={onClose}>我已保存</button>
    </>}>
      <div className="sub" style={{ margin: 0 }}>手机丢失或验证器被删除时，可以用恢复码代替验证码登录。每个只能用一次，<b>只显示这一次</b>，请保存在密码管理器或离线处。</div>
      <div className="mono" style={{ display: 'grid', gridTemplateColumns: 'repeat(2, minmax(0, 1fr))', gap: 8, marginTop: 12, fontSize: 15 }}>
        {codes.map((c) => <div key={c} style={{ background: 'var(--bg-2, var(--bg))', borderRadius: 8, padding: '8px 10px', textAlign: 'center' }}>{c}</div>)}
      </div>
    </Modal>
  )
}

type Session = { id: string; username: string; via: string; ip: string; device: string; lastSeen: string; createdAt: string; current: boolean }
const viaLabel: Record<string, string> = { password: '密码', github: 'GitHub', telegram: 'Telegram' }

// Sessions lists signed-in browsers; any can be ended.
export function Sessions() {
  const list = useLoad(() => get<Session[]>('/auth/sessions'))
  const { busy, run } = useAction()
  const confirm = useConfirm()
  const ss = list.data ?? []
  return (
    <section className="card" style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
      <div className="ctitle"><span>登录中的设备</span>
        {ss.length > 1 && <button className="btn sm danger" type="button" disabled={busy} onClick={async () => {
          if (await confirm('结束除当前设备外的所有登录？', { danger: true, ok: '全部结束' })) run(async () => { await post('/auth/sessions/revoke-others'); list.reload() }, '已结束其他登录')
        }}>结束其他所有登录</button>}
      </div>
      <div className="sub" style={{ margin: 0 }}>每次登录都是一个会话；看到不认识的设备或地址，立即结束它并修改密码。</div>
      <div style={{ display: 'grid', gap: 8 }}>
        {ss.map((s) => (
          <div key={s.id} className="row" style={{ flexWrap: 'nowrap', gap: 10, borderTop: '1px solid var(--line)', paddingTop: 8 }}>
            <div style={{ flex: 1, minWidth: 0 }}>
              <div style={{ fontWeight: 500 }}>{s.device || '未知设备'}{s.current && <span className="tag ok" style={{ marginLeft: 6 }}>当前</span>}</div>
              <div className="hint mono">{s.username} · {viaLabel[s.via] ?? s.via} · {s.ip} · 活动于 {ago(s.lastSeen)}</div>
            </div>
            {!s.current && <button className="btn sm" type="button" disabled={busy} onClick={() => run(async () => { await del('/auth/sessions/' + s.id); list.reload() }, '已结束')}>结束</button>}
          </div>
        ))}
      </div>
    </section>
  )
}

type Ban = { ip: string; failures: number; bans: number; until?: string; reason: string; updatedAt: string }
type Info = { bans: Ban[]; allowlist: string; banFailures: number; trustedProxies: string[]; yourIp: string }

// LoginProtection: per-IP bans, the allowlist and proxy trust.
export function LoginProtection() {
  const info = useLoad(() => get<Info>('/security'))
  const [f, setF] = useState({ allow: '', fails: '' })
  const { busy, run } = useAction()
  const d = info.data
  useEffect(() => { if (d) setF({ allow: d.allowlist, fails: String(d.banFailures) }) }, [d])
  const banned = (d?.bans ?? []).filter((b) => b.until && new Date(b.until) > new Date())
  return (
    <section className="card" style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
      <h2>登录防护</h2>
      <div className="sub" style={{ margin: 0 }}>同一 IP 15 分钟内登录失败达到次数即封禁，首次 15 分钟，之后每次翻倍（最长 7 天），重启不会清除；账号本身失败 5 次锁定 15 分钟。白名单内的地址永不封禁。</div>
      <div className="form-grid">
        <Field label="封禁阈值（次）"><input className="input mono" inputMode="numeric" value={f.fails} onChange={(e) => setF({ ...f, fails: e.target.value.replace(/\D/g, '') })} /></Field>
        <Field label="登录白名单" hint={`IP 或网段，逗号 / 换行分隔。你现在的地址：${d?.yourIp ?? '…'}`}>
          <textarea className="input mono" style={{ minHeight: 60 }} value={f.allow} placeholder={d?.yourIp} onChange={(e) => setF({ ...f, allow: e.target.value })} />
        </Field>
      </div>
      <div><button className="btn" type="button" disabled={busy} onClick={() => run(async () => { await put('/settings', { login_allowlist: f.allow.trim(), login_ban_failures: f.fails || '10' }); info.reload() }, '已保存')}>保存</button></div>
      <div className="hint">真实访客 IP：只信任来自 {d?.trustedProxies.length ? d.trustedProxies.join('、') : '本机与内网地址'} 的反向代理转发的 X-Forwarded-For；放在其他代理或 CDN 后面时，在 polaris.env 设置 POLARIS_TRUSTED_PROXIES。</div>
      {banned.length > 0 && <>
        <h2 style={{ fontSize: 14 }}>已封禁（{banned.length}）</h2>
        {banned.map((b) => (
          <div key={b.ip} className="row" style={{ flexWrap: 'nowrap', gap: 10 }}>
            <div style={{ flex: 1, minWidth: 0 }}>
              <div className="mono" style={{ fontWeight: 500 }}>{b.ip}</div>
              <div className="hint">{b.reason} · 第 {b.bans} 次 · 至 {new Date(b.until!).toLocaleString()}</div>
            </div>
            <button className="btn sm" type="button" disabled={busy} onClick={() => run(async () => { await del('/security/bans/' + encodeURIComponent(b.ip)); info.reload() }, '已解封')}>解封</button>
          </div>
        ))}
      </>}
    </section>
  )
}
