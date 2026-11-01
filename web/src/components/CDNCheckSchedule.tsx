import { useState } from 'react'
import { put } from '../api'
import { datetime } from '../format'
import { t } from '../i18n'
import { Field, Modal, useAction } from './ui'

export type CDNSchedule = { intervalMinutes: number; running: boolean; nextAt?: string; startedAt?: string; lastAt?: string; finishedAt?: string; checked: number; total: number; failed: number; partial: number; error?: string }

export function CDNCheckSummary({ schedule }: { schedule?: CDNSchedule }) {
  if (!schedule?.intervalMinutes) return null
  return <div className="note small cdn-check-schedule" role="status">
    <div>{schedule.running ? t('定时检测进行中') : t('定时检测：每 {n} 分钟', { n: schedule.intervalMinutes })} · {t('本轮已检 {done} / {total}', { done: schedule.checked, total: schedule.total })} · {t('失败 {n}', { n: schedule.failed })}{schedule.partial > 0 && ' · ' + t('部分完成 {n}', { n: schedule.partial })}</div>
    {!schedule.running && schedule.nextAt && <div className="mute">{t('下次检查 / 续批')}：{datetime(schedule.nextAt)}</div>}
    {schedule.lastAt && <div className="mute">{t('最近自动检测')}：{datetime(schedule.lastAt)}</div>}
    {schedule.error && <div role="alert">{schedule.error}</div>}
  </div>
}

export default function CDNCheckSchedule({ profileId, schedule, onClose, onSaved }: { profileId: string; schedule?: CDNSchedule; onClose: () => void; onSaved: () => void }) {
  const [interval, setInterval] = useState(schedule?.intervalMinutes ?? 0)
  const { busy, run } = useAction()
  const choices = [...new Set([0, 5, 15, 30, 60, 360, 1440, interval])].sort((a, b) => a - b)
  return <Modal title={t('CDN 地址定时检测')} onClose={onClose} footer={<>
    <button className="btn" type="button" disabled={busy} onClick={onClose}>{t('取消')}</button>
    <button className="btn pri" type="button" disabled={busy} onClick={() => run(async () => { await put(`/cdn-profiles/${profileId}/check-schedule`, { intervalMinutes: interval }); onSaved() }, t('定时检测设置已保存'))}>{t('保存')}</button>
  </>}>
    <Field label={t('自动检测周期')}><select className="input" aria-label={t('自动检测周期')} value={interval} onChange={(e) => setInterval(Number(e.target.value))}>{choices.map((n) => <option key={n} value={n}>{n ? t('每 {n} 分钟', { n }) : t('关闭')}</option>)}</select></Field>
    <p className="hint">{t('覆盖当前配置档所有已启用的关联入口，按实际协议、端口、SNI、Host 和路径分别检测；跳过手动禁用地址。')}</p>
    <p className="hint">{t('后台每分钟调度，地址较多时分批续检，每批最多 8 个；整轮完成后再等待设置的周期。')}</p>
    <p className="hint">{t('检测来自面板网络，只验证 DNS、TCP、TLS 和传输握手。失败地址从对应入口订阅中排除，复检通过才恢复；部分完成不会解除已有失败。')}</p>
    <p className="hint">{t('关闭或修改周期会中止旧轮次的后续写入；已有检测结论保留。')}</p>
  </Modal>
}
