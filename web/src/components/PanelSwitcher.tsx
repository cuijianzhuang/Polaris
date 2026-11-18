import { get } from '../api'
import { useLoad } from './ui'
import { Icon } from './Icons'
import { t } from '../i18n'

// PanelSwitcher is the "which panel am I on" control in the workspace bar.
//
// Members have their own logins, so switching cannot be a silent context
// swap the way a single-tenant console does it: the target is a different
// origin with a different session. What this saves is remembering the
// addresses — the list is already registered under 多机, so the switcher
// turns "open the hub, find the row, copy the URL" into one click, opening
// the target the same way the 多机 page does (new tab, no iframe).
//
// It renders nothing until at least one member is registered: a fresh
// single-panel install should not grow a switcher with one dead entry.

interface SwitchPanel {
  id: string; name: string; baseUrl: string
  enabled: boolean
  health: 'ok' | 'stale' | 'failed' | 'never'
}

interface SwitchList { panels: SwitchPanel[] }

// healthDot maps the member's health to the dot colour, so a broken member
// is visible in the list before it is opened.
const healthDot: Record<SwitchPanel['health'], string> = {
  ok: 'ok', stale: 'warn', failed: 'bad', never: '',
}

export default function PanelSwitcher({ inSidebar }: { inSidebar?: boolean }) {
  // Same endpoint as the 多机 page; the response is cached by the hub and
  // never triggers a remote probe, so reading it here is cheap.
  const list = useLoad((signal) => get<SwitchList>('/panels', signal))
  const data = list.error || !list.data || Array.isArray(list.data) ? undefined : list.data
  const panels = (data?.panels ?? []).filter((p) => p.enabled && p.baseUrl)

  // A menu with one entry is worse than no menu: the switcher only appears
  // once there is somewhere else to go.
  if (panels.length < 2) return null

  if (inSidebar) {
    // Phones hide the workspace bar entirely and navigate from the sidebar
    // drawer, so the same list has to live there.
    return (
      <details className="menu panel-switcher side-switcher">
        <summary className="nav side-switcher-summary" aria-label={t('切换面板')}>
          <Icon.Monitor />
          <span className="side-switcher-label">{t('切换面板')}</span>
          <span className="workspace-switch-count">{panels.length}</span>
        </summary>
        <div className="menu-list side-switcher-list" role="group" aria-label={t('切换面板')}>
          {panels.map((p) => (
            <a key={p.id} className="panel-switch-item" href={p.baseUrl} target="_blank" rel="noreferrer"
              title={t('切换到 {name}（在新标签页打开，需要单独登录）', { name: p.name })}>
              <span className={'dot ' + healthDot[p.health]} aria-hidden="true" />
              <span className="panel-switch-name">{p.name}</span>
              <span className="panel-switch-url mono">{hostOf(p.baseUrl)}</span>
            </a>
          ))}
        </div>
      </details>
    )
  }

  return (
    <details className="menu panel-switcher bar-switcher">
      <summary className="workspace-switch" aria-label={t('切换面板')} title={t('切换面板')}>
        <Icon.Monitor />
        <span className="workspace-switch-label">{t('切换面板')}</span>
        <span className="workspace-switch-count">{panels.length}</span>
      </summary>
      <div className="menu-list" role="group" aria-label={t('切换面板')}>
        {panels.map((p) => (
          <a key={p.id} className="panel-switch-item" href={p.baseUrl} target="_blank" rel="noreferrer"
            title={t('切换到 {name}（在新标签页打开，需要单独登录）', { name: p.name })}>
            <span className={'dot ' + healthDot[p.health]} aria-hidden="true" />
            <span className="panel-switch-name">{p.name}</span>
            <span className="panel-switch-url mono">{hostOf(p.baseUrl)}</span>
            <Icon.Chevron />
          </a>
        ))}
      </div>
    </details>
  )
}

// hostOf shows just the host: the full URL rarely fits and adds nothing
// once the member has a name.
function hostOf(raw: string): string {
  try { return new URL(raw).host } catch { return raw }
}
