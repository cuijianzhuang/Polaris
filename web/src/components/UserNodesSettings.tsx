import { useEffect, useState } from 'react'
import { get, put } from '../api'
import { t } from '../i18n'
import { SettingsSaveBar, useSettingsDraft, useSettingsSave } from './SettingsDrafts'
import { Field, Switch, useLoad } from './ui'
import { ProTag, useFeature } from './License'

// User-nodes card (general settings tab): how registered users may bring
// their own VPS into the panel. The mode lives in the plain settings store
// (user_nodes_mode); the backend resolves the effective mode, downgrading
// "open" to approval when the licence lacks the user_nodes_open feature.
const modes = [
  { v: 'off', label: () => t('关闭') },
  { v: 'approval', label: () => t('审批制') },
  { v: 'open', label: () => t('开放制') },
]

export function UserNodesSettings() {
  const info = useLoad((signal) => get<Record<string, any>>('/settings', signal))
  const openOK = useFeature('user_nodes_open')
  const [mode, setMode] = useState('')
  const [baseline, setBaseline] = useState('')
  const [memberLogin, setMemberLogin] = useState(false)
  const [memberBaseline, setMemberBaseline] = useState<boolean | null>(null)
  const saver = useSettingsSave()
  useEffect(() => {
    if (info.data) {
      const v = info.data.user_nodes_mode || 'approval'
      setMode(v); setBaseline(v)
      const ml = info.data.panel_member_login === 'true'
      setMemberLogin(ml); setMemberBaseline(ml)
    }
  }, [info.data])
  const dirty = (!!baseline && mode !== baseline) || (memberBaseline !== null && memberLogin !== memberBaseline)
  const save = async () => {
    if (!dirty) return true
    if (saver.busy || info.loading) return false
    return saver.run(async () => { await put('/settings', { user_nodes_mode: mode, panel_member_login: String(memberLogin) }); setBaseline(mode); setMemberBaseline(memberLogin); info.reload() })
  }
  useSettingsDraft({ label: t('用户节点'), dirty, busy: saver.busy, save, discard: () => { if (baseline) setMode(baseline); if (memberBaseline !== null) setMemberLogin(memberBaseline) } })
  return (
    <section className="card ops-stack">
      <div className="ctitle"><h2>{t('用户节点')}</h2></div>
      <div className="hint">{t('注册用户可把自己的 VPS 接入面板自助管理；节点只服务归属用户自己的账号，不进公共订阅池。社区版每用户最多 3 台，付费许可证可放宽上限。')}</div>
      {info.error && <div className="note warn" role="alert">{info.error}</div>}
      <fieldset className="settings-fields" disabled={saver.busy || info.loading || !baseline}>
        <Field label={t('接入模式')}>
          <div className="seg">
            {modes.map((m) => (
              <button key={m.v} type="button" className={'opt' + (mode === m.v ? ' on' : '')} aria-pressed={mode === m.v} onClick={() => setMode(m.v)}>
                {m.label()}{m.v === 'open' && <ProTag on={openOK} />}
              </button>
            ))}
          </div>
        </Field>
        {mode === 'off' && <div className="hint">{t('关闭后用户不能再接入新节点；已接入的节点不受影响，仍可管理与删除。')}</div>}
        {mode === 'approval' && <div className="hint">{t('用户接入的节点先待审批：能上线供你核对机器信息，但不下发配置、不被订阅使用，你批准后才生效。')}</div>}
        {mode === 'open' && (openOK
          ? <div className="hint">{t('用户接入的节点立即生效，不经审批。')}</div>
          : <div className="note warn">{t('开放制需专业版许可：当前许可不含「用户节点开放接入」，保存后实际按审批制生效。')}</div>)}
        <div className="toggle"><span>{t('注册用户登录面板：开启后注册用户可用邮箱验证码登录面板，只能查看和运维自己的节点')}</span><Switch label={t('注册用户登录面板')} on={memberLogin} onChange={setMemberLogin} /></div>
        {memberLogin && mode === 'off' && <div className="note warn">{t('接入模式为「关闭」时，注册用户登录面板不可用。')}</div>}
        <SettingsSaveBar loading={info.loading} dirty={dirty} busy={saver.busy} error={saver.error || info.error} disabled={info.loading || !info.data} onSave={save} />
      </fieldset>
    </section>
  )
}
