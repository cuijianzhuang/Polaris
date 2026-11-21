import { useEffect, useState } from 'react'
import { get, put } from '../api'
import { t } from '../i18n'
import { SettingsSaveBar, useSettingsDraft, useSettingsSave } from './SettingsDrafts'
import { Field, useLoad } from './ui'

// Resource alerts threshold form (notifications tab). Values live in the
// plain settings store (resource_alert_*); the evaluator on the leader
// reads them each round, so saving takes effect without a restart.
export function ResourceAlertSettings() {
  const info = useLoad((signal) => get<Record<string, any>>('/settings', signal))
  const [f, setF] = useState<Record<string, string>>({})
  const [baseline, setBaseline] = useState('')
  const saver = useSettingsSave()
  useEffect(() => {
    if (info.data) {
      const next = {
        resource_alert_enabled: info.data.resource_alert_enabled ?? 'true',
        resource_alert_cpu: info.data.resource_alert_cpu ?? '90',
        resource_alert_mem: info.data.resource_alert_mem ?? '90',
        resource_alert_disk: info.data.resource_alert_disk ?? '90',
        resource_alert_minutes: info.data.resource_alert_minutes ?? '15',
      }
      setF(next); setBaseline(JSON.stringify(next))
    }
  }, [info.data])
  const set = (k: string, v: string) => setF((x) => ({ ...x, [k]: v }))
  const dirty = !!baseline && JSON.stringify(f) !== baseline
  const pctOK = (v: string) => { const n = Number(v); return v.trim() !== '' && Number.isFinite(n) && n >= 50 && n <= 100 }
  const minutes = Number(f.resource_alert_minutes)
  const invalid = !pctOK(f.resource_alert_cpu ?? '') || !pctOK(f.resource_alert_mem ?? '') || !pctOK(f.resource_alert_disk ?? '') ||
    !Number.isInteger(minutes) || minutes < 5 || minutes > 120
  const save = async () => {
    if (!dirty) return true
    if (invalid || saver.busy || info.loading) return false
    return saver.run(async () => { await put('/settings', f); setBaseline(JSON.stringify(f)); info.reload() })
  }
  useSettingsDraft({ label: t('资源告警'), dirty, busy: saver.busy, invalid, save, discard: () => { if (baseline) setF(JSON.parse(baseline)) } })
  return (
    <section className="card ops-stack">
      <div className="ctitle"><h2>{t('资源告警')}</h2></div>
      <div className="hint">{t('节点 CPU、内存或磁盘持续超阈值时发送告警，恢复后发送恢复通知；按 5 分钟粒度评估，只有面板主节点发送。')}</div>
      {info.error && <div className="note warn" role="alert">{info.error}</div>}
      <fieldset className="settings-fields" disabled={saver.busy || info.loading || !baseline}>
        <label className="chk"><input type="checkbox" checked={f.resource_alert_enabled !== 'false'} onChange={(e) => set('resource_alert_enabled', e.target.checked ? 'true' : 'false')} />{t('启用资源告警')}</label>
        <div className="form-grid">
          <Field label={t('CPU 阈值（%）')}><input className="input mono" inputMode="numeric" value={f.resource_alert_cpu ?? ''} onChange={(e) => set('resource_alert_cpu', e.target.value)} /></Field>
          <Field label={t('内存阈值（%）')}><input className="input mono" inputMode="numeric" value={f.resource_alert_mem ?? ''} onChange={(e) => set('resource_alert_mem', e.target.value)} /></Field>
          <Field label={t('磁盘阈值（%）')}><input className="input mono" inputMode="numeric" value={f.resource_alert_disk ?? ''} onChange={(e) => set('resource_alert_disk', e.target.value)} /></Field>
          <Field label={t('持续时长（分钟）')} hint={t('阈值 50–100，时长 5–120')}><input className="input mono" inputMode="numeric" value={f.resource_alert_minutes ?? ''} onChange={(e) => set('resource_alert_minutes', e.target.value)} /></Field>
        </div>
        <SettingsSaveBar loading={info.loading} dirty={dirty} busy={saver.busy} error={saver.error || info.error} disabled={info.loading || !info.data || invalid} onSave={save} />
      </fieldset>
    </section>
  )
}
