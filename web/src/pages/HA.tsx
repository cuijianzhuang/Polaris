import { useState } from 'react'
import { del, get, post } from '../api'
import { Copy, CopyBox, Empty, Field, useAction, useConfirm, useLoad } from '../components/ui'
import { ago, bytes } from '../format'
import { t } from '../i18n'

// HA page (PR3): status, WireGuard peers, join token, Cloudflare token and
// the manual failover wizard. It only reads the PR1 APIs; nothing here
// executes a promotion or touches DNS — the wizard only checks and hands
// out commands for a human to run.

interface HAMember {
  id: string; host: string; version: string; edition: string
  online: boolean; leader: boolean; self: boolean
  seenAt: string; startedAt: string
}

interface HAReplica {
  clientAddr: string; state: string; syncState: string; appName: string
  lagBytes: number | null
}

interface HAStatus {
  role: 'none' | 'primary' | 'secondary'
  members: HAMember[]; leader: string; shared: boolean
  replication: HAReplica[] | null
  inRecovery: boolean | null; replayLagSeconds: number | null
  syncNames: string; syncWaiting: boolean
  redisConfigured: boolean; version: string; edition: string
}

interface HAPeer {
  wgIp: string; pubkey: string; endpoint: string; role: string
  createdAt: string; lastSeen: string | null
}

interface HAPeers { peers: HAPeer[] }

interface HAToken { token: string; command: string; expiresIn: number }

interface FailoverCheck {
  role: string; healthy: boolean; inRecovery: boolean | null
  replayLagSeconds: number | null; dnsHost: string
  dnsAddresses: string[] | null; primaryReachable: boolean | null
  primaryStatus: number | null; steps: string[]
}

const roleMeta: Record<HAStatus['role'], { cls: string; label: string }> = {
  primary: { cls: 'ok', label: t('主节点') },
  secondary: { cls: 'warn', label: t('备节点') },
  none: { cls: '', label: t('未配置') },
}

function pgRoleText(inRecovery: boolean | null): string {
  if (inRecovery === true) return t('从库')
  if (inRecovery === false) return t('主库')
  return t('未知')
}

export default function HA() {
  const status = useLoad((signal) => get<HAStatus>('/ha/status', signal))
  const peers = useLoad((signal) => get<HAPeers>('/ha/peers', signal))
  const confirm = useConfirm()
  const { busy, run } = useAction()
  const [issued, setIssued] = useState<HAToken | null>(null)
  const [cfToken, setCfToken] = useState('')
  const [cfSaved, setCfSaved] = useState(false)
  const [check, setCheck] = useState<FailoverCheck | null>(null)
  const [done, setDone] = useState<boolean[]>([])
  const [syncBusy, setSyncBusy] = useState(false)

  // The audit fixture answers unknown endpoints with []; tolerate that
  // shape instead of crashing on a missing field.
  const st: HAStatus | undefined = status.error || !status.data || Array.isArray(status.data) ? undefined : status.data
  const peerData = peers.error ? undefined : peers.data
  const peerList: HAPeer[] = Array.isArray(peerData) ? peerData : (peerData?.peers ?? [])
  const members = st?.members ?? []
  const leaderMember = members.find((m) => m.leader) ?? members.find((m) => m.id === st?.leader)
  const role = st?.role ?? 'none'
  const meta = roleMeta[role]
  const steps = check?.steps ?? []
  const doneCount = done.filter(Boolean).length

  const reload = () => { status.reload(); peers.reload() }
  const issueToken = () => run(async () => {
    const r = await post<HAToken>('/ha/token')
    setIssued(r)
  }, t('已生成'))
  const saveCFToken = () => run(async () => {
    await post('/ha/cf-token', { token: cfToken.trim() })
    setCfToken('')
    setCfSaved(true)
  }, t('已保存'))
  const runCheck = () => run(async () => {
    const r = await post<FailoverCheck>('/ha/failover-check')
    setCheck(r)
    setDone((r.steps ?? []).map(() => false))
  })
  // Toggle synchronous replication on the primary. The server derives the
  // standby name from ha_peers, so the page only sends intent.
  const toggleSync = async (enabled: boolean) => {
    if (enabled && !await confirm(t('开启后每次写库都要等备机确认：延迟增加，但故障时不丢已提交的写入。确认开启？'))) return
    setSyncBusy(true)
    try {
      await post('/ha/sync-replication', { enabled })
      status.reload()
    } finally {
      setSyncBusy(false)
    }
  }
  const removePeer = async (p: HAPeer) => {
    if (!await confirm(t('确定删除这个对端吗？仅删除库里的记录，WireGuard 配置由同步任务对齐。'), { danger: true })) return
    run(async () => { await del(`/ha/peers/${encodeURIComponent(p.wgIp)}`); peers.reload() }, t('已删除'))
  }

  return (
    <>
      <div className="top">
        <div><h1>{t('高可用')}</h1><div className="sub">{t('同一面板集群的主备实例、复制状态与故障切换向导')}</div></div>
        <div className="row">
          <button className="btn" type="button" onClick={reload} disabled={status.loading || peers.loading}>{t('刷新')}</button>
        </div>
      </div>
      {status.error && <div className="note bad" role="alert">{t('高可用状态加载失败，请重试')} <button className="link" type="button" disabled={status.loading} onClick={() => status.reload()}>{t('重试')}</button></div>}
      {peers.error && <div className="note bad" role="alert">{t('对端列表加载失败，请重试')} <button className="link" type="button" disabled={peers.loading} onClick={() => peers.reload()}>{t('重试')}</button></div>}
      <div className="spane">
        {st && role === 'none' && <div className="note">{t('当前未配置高可用。请先在主节点运行 install-panel.sh ha-init，再用本页生成加入命令。')}</div>}
        {st && (
          <section className="card">
            <h2>{t('状态')}</h2>
            <dl className="kv">
              <dt>{t('角色')}</dt><dd><span className={'tag ' + meta.cls}>{meta.label}</span></dd>
              <dt>{t('当前 Leader')}</dt><dd>{leaderMember ? leaderMember.host : t('未知')}</dd>
              <dt>{t('数据库角色')}</dt><dd>{pgRoleText(st.inRecovery)}{st.inRecovery === null && <span className="mute"> · {t('SQLite 下不可用，需 PostgreSQL')}</span>}</dd>
              <dt>{t('回放延迟')}</dt><dd>{st.replayLagSeconds === null ? t('未知') : t('{n} 秒', { n: st.replayLagSeconds.toFixed(1) })}</dd>
              <dt>{t('Redis')}</dt><dd>{st.redisConfigured ? t('已配置') : t('未配置')}</dd>
              <dt>{t('面板版本')}</dt><dd><span className="mono">{st.version || t('未知')}</span>{st.edition ? <span className="mute"> · {st.edition}</span> : null}</dd>
              <dt>{t('实例间共享')}</dt><dd>{st.shared ? t('已启用') : t('未启用')}</dd>
            </dl>
          </section>
        )}
        {st && (
          <section className="card flush">
            <h2 style={{ padding: '16px 20px 0' }}>{t('实例')}</h2>
            {members.length === 0 ? (
              <Empty>{t('还没有实例记录')}</Empty>
            ) : (
              <div className="scroll">
                <table className="rtable">
                  <thead><tr><th>{t('实例')}</th><th>{t('版本')}</th><th>{t('状态')}</th><th>Leader</th><th>{t('本机')}</th><th>{t('最近可见')}</th></tr></thead>
                  <tbody>
                    {members.map((m) => (
                      <tr key={m.id}>
                        <td style={{ fontWeight: 500 }}>{m.host || m.id}{m.self && <span className="tag" style={{ marginLeft: 6 }}>{t('本机')}</span>}</td>
                        <td><span className="mono">{m.version || t('未知')}</span>{m.edition ? <div className="small mute">{m.edition}</div> : null}</td>
                        <td><span className={'tag ' + (m.online ? 'ok' : 'bad')}>{m.online ? t('在线') : t('离线')}</span></td>
                        <td>{m.leader ? <span className="tag ok">Leader</span> : '—'}</td>
                        <td>{m.self ? t('是') : '—'}</td>
                        <td>{ago(m.seenAt)}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
          </section>
        )}
        {st && (
          <section className="card">
            <h2>{t('PostgreSQL 复制')}</h2>
            {st.replication === null ? (
              <div className="note">{t('复制状态不可用（需 PostgreSQL）')}</div>
            ) : st.replication.length === 0 ? (
              <div className="note">{t('无从库连接')}</div>
            ) : (
              <div className="scroll">
                <table className="rtable">
                  <thead><tr><th>{t('客户端地址')}</th><th>{t('状态')}</th><th>{t('同步状态')}</th><th>{t('延迟')}</th></tr></thead>
                  <tbody>
                    {st.replication.map((r) => (
                      <tr key={r.clientAddr}>
                        <td className="mono">{r.clientAddr}</td>
                        <td>{r.state}</td>
                        <td>{r.syncState}</td>
                        <td className="mono">{r.lagBytes === null ? '—' : bytes(r.lagBytes)}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
            <div className="row" style={{ padding: '12px 20px 16px', gap: 12, alignItems: 'center', flexWrap: 'wrap' }}>
              {role !== 'primary' ? (
                <span className="mute">{t('同步复制只在主节点上配置。')}</span>
              ) : (
                <>
                  <span className={'tag ' + (st.syncWaiting ? 'ok' : st.syncNames ? 'warn' : '')}>
                    {st.syncWaiting ? t('同步复制已生效') : st.syncNames ? t('已设置但未生效') : t('异步复制')}
                  </span>
                  <button className="btn" type="button" disabled={syncBusy || st.syncWaiting || peerList.length === 0}
                    onClick={() => toggleSync(true)}>{t('开启同步复制')}</button>
                  <button className="btn" type="button" disabled={syncBusy || !st.syncNames}
                    onClick={() => toggleSync(false)}>{t('恢复异步')}</button>
                  <span className="mute">
                    {st.syncNames
                      ? t('当前：{n}', { n: st.syncNames })
                      : t('异步复制：故障瞬间可能丢几秒写入；同步复制要付出每次写库多一次往返的代价。')}
                  </span>
                </>
              )}
            </div>
          </section>
        )}
        <section className="card flush">
          <h2 style={{ padding: '16px 20px 0' }}>{t('WireGuard 对端')}</h2>
          {peerList.length === 0 ? (
            <Empty>{t('还没有对端')}</Empty>
          ) : (
            <div className="scroll">
              <table className="rtable">
                <thead><tr><th>WG IP</th><th>{t('公钥')}</th><th>{t('对端地址')}</th><th>{t('角色')}</th><th>{t('最近可见')}</th><th></th></tr></thead>
                <tbody>
                  {peerList.map((p) => (
                    <tr key={p.wgIp}>
                      <td className="mono">{p.wgIp}</td>
                      <td style={{ maxWidth: 220 }}><span className="mono" style={{ display: 'block', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }} title={p.pubkey}>{p.pubkey}</span></td>
                      <td className="mono" style={{ overflowWrap: 'anywhere' }}>{p.endpoint || '—'}</td>
                      <td>{p.role === 'primary' ? t('主节点') : p.role === 'secondary' ? t('备节点') : p.role}</td>
                      <td>{ago(p.lastSeen)}</td>
                      <td style={{ textAlign: 'right', whiteSpace: 'nowrap' }}>
                        <button className="link danger" type="button" disabled={busy} onClick={() => removePeer(p)}>{t('删除')}</button>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
          <div className="small mute" style={{ padding: '12px 20px 16px' }}>{t('本机 WG 地址未在接口中返回，暂不可用。')}</div>
        </section>
        <section className="card" style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
          <h2>{t('加入备节点')}</h2>
          <div className="sub" style={{ margin: 0 }}>{t('生成一条加入命令，在备节点上执行即可完成加入。')}</div>
          {issued ? (
            <>
              <CopyBox text={issued.command} />
              <div className="note warn">{t('加入命令只显示一次，有效期 {n} 分钟；生成新 token 会作废旧 token。', { n: Math.max(1, Math.round(issued.expiresIn / 60)) })}</div>
              <div className="row"><button className="btn" type="button" onClick={() => setIssued(null)}>{t('隐藏')}</button></div>
            </>
          ) : (
            <div className="row">
              <button className="btn pri" type="button" disabled={busy} onClick={issueToken}>{t('生成加入命令')}</button>
            </div>
          )}
          <div className="small mute">{t('敏感操作，首次执行时会要求二次验证。')}</div>
        </section>
        <section className="card" style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
          <h2>{t('Cloudflare Token')}</h2>
          <div className="sub" style={{ margin: 0 }}>{t('用于 DNS 切换与证书签发。')}</div>
          {cfSaved && <div className="note ok-note">{t('已配置')}：{t('Cloudflare Token 已保存，面板不会回显 token 内容。')}</div>}
          <Field label={t('Cloudflare API Token')}>
            <input className="input mono" type="password" autoComplete="off" value={cfToken}
              placeholder="Cloudflare API Token"
              onChange={(e) => { setCfToken(e.target.value); setCfSaved(false) }} />
          </Field>
          <div className="row">
            <button className="btn pri" type="button" disabled={busy || !cfToken.trim()} onClick={saveCFToken}>{t('保存')}</button>
          </div>
          <div className="small mute">{t('保存后无法查看，只能重新保存覆盖。敏感操作，首次执行时会要求二次验证。')}</div>
        </section>
        <section className="card" style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
          <h2>{t('故障切换向导')}</h2>
          <div className="sub" style={{ margin: 0 }}>{t('只做检查与给出命令，不代劳执行。提主与 DNS 回切永远是人工操作。')}</div>
          {st && st.syncWaiting
            ? <div className="note">{t('同步复制已开启：故障时不丢已提交的写入。')}</div>
            : <div className="note warn">{t('异步复制：故障瞬间可能丢几秒写入。')}</div>}
          {role === 'primary' && <div className="note">{t('当前是主节点：向导在备节点上使用才有意义。')}</div>}
          <div className="row">
            <button className="btn pri" type="button" disabled={busy} onClick={runCheck}>{check ? t('重新检查') : t('开始检查')}</button>
          </div>
          {check && (
            <>
              <dl className="kv">
                <dt>{t('检查结果')}</dt><dd><span className={'tag ' + (check.healthy ? 'ok' : 'bad')}>{check.healthy ? t('检查通过') : t('检查异常')}</span></dd>
                <dt>{t('主站可达')}</dt><dd>{check.primaryReachable === null ? t('未知') : check.primaryReachable ? t('可达') : t('不可达')}{check.primaryStatus !== null && <span className="mute"> · HTTP {check.primaryStatus}</span>}</dd>
                <dt>{t('回放延迟')}</dt><dd>{check.replayLagSeconds === null ? t('未知') : t('{n} 秒', { n: check.replayLagSeconds.toFixed(1) })}</dd>
                <dt>{t('DNS 当前指向')}</dt><dd><span className="mono" style={{ overflowWrap: 'anywhere' }}>{check.dnsHost || t('未知')}</span>{check.dnsAddresses && check.dnsAddresses.length > 0 && <div className="mono small" style={{ overflowWrap: 'anywhere' }}>{check.dnsAddresses.join(', ')}</div>}</dd>
                <dt>{t('数据库角色')}</dt><dd>{pgRoleText(check.inRecovery)}</dd>
              </dl>
              <h3>{t('提主步骤')} · {t('已完成 {n} / {total} 步', { n: doneCount, total: steps.length })}</h3>
              {steps.length === 0 ? (
                <div className="note">{t('暂无步骤')}</div>
              ) : steps.map((s, i) => (
                <div key={i} style={{ borderTop: '1px solid var(--line)', padding: '12px 0' }}>
                  <label className="row" style={{ gap: 10, alignItems: 'flex-start', flexWrap: 'wrap' }}>
                    <input type="checkbox" checked={!!done[i]} onChange={(e) => setDone(done.map((d, j) => j === i ? e.target.checked : d))}
                      style={{ width: 18, height: 18, flex: 'none', marginTop: 2 }} aria-label={t('第 {n} 步', { n: i + 1 })} />
                    <span style={{ flex: 1, minWidth: 200 }}>{i + 1}. {s}</span>
                    <Copy text={s} />
                  </label>
                </div>
              ))}
            </>
          )}
        </section>
      </div>
    </>
  )
}
