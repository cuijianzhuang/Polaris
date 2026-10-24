import { useState } from 'react'
import { get } from '../api'
import { Field, useLoad } from './ui'
import { t } from '../i18n'

export default function SnellOwner({ value, onChange }: { value: string; onChange: (id: string) => void }) {
  const [q, setQ] = useState('')
  const users = useLoad(() => get<{ items: { id: string; username: string }[] }>(`/users?limit=20&q=${encodeURIComponent(q)}`), [q])
  const items = users.data?.items ?? []
  return <>
    <Field label={t('搜索 Snell 专属用户')} htmlFor="snell-user-search">
      <input id="snell-user-search" className="input" value={q} onChange={(e) => setQ(e.target.value)} />
    </Field>
    <Field label={t('Snell 专属用户')} htmlFor="snell-user" hint={t('选择用户不会额外授予访问权；仍需通过分组、节点或入站授权。停用、到期或超额时关闭该监听。')}>
      <select id="snell-user" className="input" value={value} onChange={(e) => onChange(e.target.value)} required>
        <option value="">{t('请选择用户')}</option>
        {value && !items.some((u) => u.id === value) && <option value={value}>{value}</option>}
        {items.map((u) => <option key={u.id} value={u.id}>{u.username} · {u.id}</option>)}
      </select>
      {users.error && <div className="error">{String(users.error)}</div>}
    </Field>
  </>
}
