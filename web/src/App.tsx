import StaffWorkspace, { StaffAccount } from './pages/StaffWorkspace'
import { Component, Suspense, createContext, lazy, useCallback, useContext, useEffect, useLayoutEffect, useState, type ComponentType, type ReactNode } from 'react'
import { createBrowserRouter, RouterProvider, Link, NavLink, Navigate, Route, Routes, useLocation } from 'react-router-dom'
import { BASE, get, getToken, onInflight, post, setToken, setUnauthorizedHandler, takeOAuthResult, wsURL } from './api'
import { Icon } from './components/Icons'
import CommandPalette, { type PaletteItem } from './components/CommandPalette'
import { recordRates } from './components/NodeCard'
import { receivedNetHealth } from './netHealth'
import { setPremiumThemes, useTheme } from './theme'
import { usePlugins } from './plugins'
import { useBrand } from './brand'
import { useFeature } from './components/License'
import { ensurePageDict, loadRestInBackground, onDictReady, t } from './i18n'
import LangToggle from './components/LangToggle'
import SidebarAccount from './components/SidebarAccount'
import { ConfirmProvider, LoadFailures, SkeletonRows, ToastProvider } from './components/ui'
import { useMobileKeyboard } from './components/useMobileKeyboard'
import { useMenuDismiss } from './components/useMenuDismiss'
import SudoPrompt from './components/SudoPrompt'
import { SettingsDraftProvider, useSettingsLeave } from './components/SettingsDrafts'
import type { Node } from './types'
import Login from './pages/Login'
// Pages load on demand. After a panel upgrade the old chunk names are gone:
// a failed load reloads the page once to pick up the new build.
function page<T extends { default: ComponentType }>(id: string, load: () => Promise<T>) {
  // The page chunk and its small translation dictionary are independent
  // requests. Start both immediately instead of serialising first paint.
  return lazy(() => Promise.all([ensurePageDict(id), load()]).then(([, m]) => {
    try { sessionStorage.removeItem('polaris-chunk-reload') } catch { /* storage may be blocked */ }
    return m
  }, (err) => {
    let reloaded = false
    try { reloaded = sessionStorage.getItem('polaris-chunk-reload') === '1'; sessionStorage.setItem('polaris-chunk-reload', '1') } catch { /* ignore */ }
    if (!reloaded) location.reload()
    throw err
  }))
}

const PluginPage = page('PluginPage', () => import('./pages/PluginPage'))
const Dashboard = page('Dashboard', () => import('./pages/Dashboard'))
const Stats = page('Stats', () => import('./pages/Stats'))
const Nodes = page('Nodes', () => import('./pages/Nodes'))
const NodeMap = page('NodeMap', () => import('./pages/NodeMap'))
const Probe = page('Probe', () => import('./pages/Probe'))
const Cores = page('Cores', () => import('./pages/Cores'))
const Forwards = page('Forwards', () => import('./pages/Forwards'))
const Inbounds = page('Inbounds', () => import('./pages/Inbounds'))
const Chains = page('Chains', () => import('./pages/Chains'))
const Templates = page('Templates', () => import('./pages/Templates'))
const Users = page('Users', () => import('./pages/Users'))
const Shop = page('Shop', () => import('./pages/Shop'))
const Tickets = page('Tickets', () => import('./pages/Tickets'))
const Plans = page('Plans', () => import('./pages/Plans'))
const SubTemplates = page('SubTemplates', () => import('./pages/SubTemplates'))
const Certificates = page('Certificates', () => import('./pages/Certificates'))
const Websites = page('Websites', () => import('./pages/Websites'))
const Settings = page('Settings', () => import('./pages/Settings'))
const Audit = page('Audit', () => import('./pages/Audit'))
const Convert = page('Convert', () => import('./pages/Convert'))
const Domains = page('Domains', () => import('./pages/Domains'))
const Telegram = page('Telegram', () => import('./pages/Telegram'))
const Costs = page('Costs', () => import('./pages/Costs'))
const LicensePage = page('LicensePage', () => import('./pages/LicensePage'))
const Extensions = page('Extensions', () => import('./pages/Extensions'))

// Scope of the session: Telegram node operators see only their own nodes.
const ScopeCtx = createContext<{ scoped: boolean }>({ scoped: false })
export function useScope() { return useContext(ScopeCtx) }

// Live node list shared by pages, fed by the WebSocket.
const NodesCtx = createContext<{ nodes: Node[]; refresh: () => void }>({ nodes: [], refresh: () => {} })
export function useNodes() { return useContext(NodesCtx) }

function NodesProvider({ children }: { children: ReactNode }) {
  const [nodes, setNodes] = useState<Node[]>([])
  const receive = useCallback((items: Node[]) => setNodes(items.map((n) => ({ ...n, netHealth: receivedNetHealth(n.netHealth) }))), [])
  const refresh = useCallback(() => { get<Node[]>('/nodes').then(receive).catch(() => {}) }, [receive])
  useEffect(() => {
    refresh()
    let ws: WebSocket | null = null
    let stop = false
    let retry: ReturnType<typeof setTimeout>
    const connect = () => {
      ws = new WebSocket(wsURL())
      ws.onmessage = (e) => {
        try {
          const m = JSON.parse(e.data)
          if (m.type === 'nodes') receive(m.data)
        } catch { /* ignore */ }
      }
      ws.onclose = () => { if (!stop) retry = setTimeout(connect, 5000) }
    }
    connect()
    return () => { stop = true; clearTimeout(retry); ws?.close() }
  }, [refresh, receive])
  useEffect(() => { recordRates(nodes) }, [nodes])
  return <NodesCtx.Provider value={{ nodes, refresh }}>{children}</NodesCtx.Provider>
}

type NavItem = { to: string; label: string; icon: ReactNode; end?: boolean; element?: ComponentType; children?: { to: string; label: string; icon: ReactNode; element?: ComponentType }[] }

// Sidebar + routes share this one table: related pages form one collapsible
// group, and every leaf carries the lazy component it renders, so the <Routes>
// below never drifts from the menu.
const nav: NavItem[] = [
  { to: '/', label: t('仪表盘'), icon: <Icon.Dashboard />, end: true, element: Dashboard },
  { to: '/stats', label: t('统计'), icon: <Icon.Chart />, element: Stats },
  { to: '/nodes', label: t('节点'), icon: <Icon.Nodes />, children: [
    { to: '/nodes', label: t('节点列表'), icon: <Icon.Server />, element: Nodes },
    { to: '/map', label: t('节点地图'), icon: <Icon.Map />, element: NodeMap },
    { to: '/probe', label: t('探针'), icon: <Icon.Pulse />, element: Probe },
    { to: '/cores', label: t('内核'), icon: <Icon.Cpu />, element: Cores },
    { to: '/costs', label: t('服务器成本'), icon: <Icon.Wallet />, element: Costs },
  ] },
  { to: '/inbounds', label: t('入站与转发'), icon: <Icon.Inbound />, children: [
    { to: '/inbounds', label: t('入站'), icon: <Icon.Inbound />, element: Inbounds },
    { to: '/templates', label: t('入站模板'), icon: <Icon.Template />, element: Templates },
    { to: '/forwards', label: t('端口转发'), icon: <Icon.Forward />, element: Forwards },
  ] },
  { to: '/chains', label: t('链路与路由'), icon: <Icon.Chain />, element: Chains },
  { to: '/users', label: t('用户'), icon: <Icon.Users />, children: [
    { to: '/users', label: t('用户'), icon: <Icon.Users />, element: Users },
    { to: '/plans', label: t('套餐'), icon: <Icon.Package />, element: Plans },
    { to: '/shop', label: t('售卖'), icon: <Icon.Wallet />, element: Shop },
    { to: '/tickets', label: t('工单'), icon: <Icon.Mail />, element: Tickets },
  ] },
  { to: '/sub-templates', label: t('订阅'), icon: <Icon.Sub />, children: [
    { to: '/sub-templates', label: t('订阅设置'), icon: <Icon.Sub />, element: SubTemplates },
    { to: '/convert', label: t('订阅转换'), icon: <Icon.Convert />, element: Convert },
  ] },
  { to: '/domains', label: t('域名与证书'), icon: <Icon.Globe />, children: [
    { to: '/domains', label: t('域名与 DDNS'), icon: <Icon.Globe />, element: Domains },
    { to: '/certificates', label: t('证书'), icon: <Icon.Lock />, element: Certificates },
    { to: '/websites', label: t('网站与 443 入口'), icon: <Icon.Globe />, element: Websites },
  ] },
  { to: '/extensions', label: t('扩展'), icon: <Icon.Extension />, element: Extensions },
  { to: '/settings', label: t('系统'), icon: <Icon.Gear />, children: [
    { to: '/settings', label: t('设置'), icon: <Icon.Gear />, element: Settings },
    { to: '/telegram', label: t('Telegram 机器人'), icon: <Icon.Bot />, element: Telegram },
    { to: '/audit', label: t('审计日志'), icon: <Icon.Log />, element: Audit },
  ] },
  { to: '/license', label: t('许可证'), icon: <Icon.Lock />, element: LicensePage },
]

// Route leaves derived from the nav table (groups expand to their children).
const routeLeaves = nav.flatMap((n) => n.children ?? [n])

// TopProgress is a thin bar shown while API requests are in flight (after a
// short delay, so fast requests do not flicker).
function TopProgress() {
  const [show, setShow] = useState(false)
  useEffect(() => {
    let t: ReturnType<typeof setTimeout> | undefined
    const off = onInflight((n) => {
      clearTimeout(t)
      if (n > 0) t = setTimeout(() => setShow(true), 150)
      else setShow(false)
    })
    return () => { off(); clearTimeout(t) }
  }, [])
  return <div className={'topbar' + (show ? ' on' : '')} aria-hidden="true" />
}

// NavGroup is a collapsible sidebar entry; it opens itself while one of its
// pages is shown.
function NavGroup({ item, folded, onUnfold }: { item: NavItem; folded: boolean; onUnfold: () => void }) {
  const loc = useLocation()
  const here = item.children!.some((c) => loc.pathname === c.to || loc.pathname.startsWith(c.to + '/'))
  const groupID = 'nav-group-' + item.to.replace(/\W/g, '-')
  const [open, setOpen] = useState(here)
  useEffect(() => { if (here) setOpen(true) }, [here])
  return (
    <div className={'navgroup' + (open ? ' open' : '') + (here ? ' current' : '')}>
      <button type="button" className={'nav' + (here && !open ? ' active' : '')} aria-expanded={open} aria-controls={groupID} title={folded ? item.label : undefined} onClick={() => { if (folded) { setOpen(true); onUnfold() } else setOpen(!open) }}>
        {item.icon}{item.label}<span className="chev" aria-hidden="true"><Icon.Chevron /></span>
      </button>
      <div className="navsub" id={groupID}>
        {item.children!.map((c) => <NavLink key={c.to} to={c.to} className="nav" title={folded ? c.label : undefined}>{c.label}</NavLink>)}
      </div>
    </div>
  )
}

// ThemeToggle cycles light → dark → follow system; accents live in 设置.
function ThemeToggle({ themesOK }: { themesOK: boolean }) {
  const [th, setTh] = useTheme()
  if (th.style !== 'default' && themesOK) return null // an exclusive theme has its own palette
  const next = { light: 'dark', dark: 'system', system: 'light' } as const
  const label = { light: t('浅色'), dark: t('深色'), system: t('跟随系统') }[th.mode]
  return (
    <button type="button" className="theme-btn" title={t('主题：{label}（点击切换）', { label })} aria-label={t('主题：{label}', { label })}
      onClick={() => setTh({ ...th, mode: next[th.mode] })}>
      {th.mode === 'light' ? <Icon.Sun /> : th.mode === 'dark' ? <Icon.Moon /> : <Icon.Monitor />}
    </button>
  )
}

// Bottom tab bar on phones: the pages used most, plus 更多 for the full menu.
const tabs = [
  { to: '/', label: t('仪表盘'), icon: <Icon.Dashboard />, end: true },
  { to: '/nodes', label: t('节点'), icon: <Icon.Server /> },
  { to: '/inbounds', label: t('入站'), icon: <Icon.Inbound /> },
  { to: '/users', label: t('用户'), icon: <Icon.Users /> },
]

// PageBoundary keeps a failing page from blanking the whole panel: the menu
// stays and the error is shown (it resets on navigation via main's key).
class PageBoundary extends Component<{ children: ReactNode }, { error: Error | null }> {
  state = { error: null as Error | null }
  static getDerivedStateFromError(error: Error) { return { error } }
  componentDidCatch(error: Error) { console.error(error) }
  render() {
    if (!this.state.error) return this.props.children
    return (
      <div className="card" style={{ display: 'flex', flexDirection: 'column', gap: 10 }}>
        <h2>{t('页面出错了')}</h2>
        <div className="note bad mono small" style={{ overflowWrap: 'anywhere' }}>{this.state.error.message}</div>
        <div><button className="btn" type="button" onClick={() => location.reload()}>{t('重新加载')}</button></div>
      </div>
    )
  }
}

// AppShellSkeleton is the first paint while the session role loads: it
// mirrors the real console shell (sidebar, top bar, content) with shimmer
// placeholders, so the page looks like it is materialising instead of a
// bare "加载中…" card on an empty page. The brand logo pulses and a
// spinner + "加载中…" line say it is loading, not stuck.
function AppShellSkeleton() {
  const { title } = useBrand()
  return (
    <div className="app" aria-busy="true" aria-label={t('加载中')}>
      <header className="mtop" aria-hidden="true">
        <span className="mtop-btn"><span className="skeleton" style={{ width: 20, height: 20, borderRadius: 6 }} /></span>
        <div className="mtop-title"><span className="sk-brand-logo"><Icon.Brand size={26} /></span><span>{title}</span></div>
        <span className="mtop-btn"><span className="skeleton" style={{ width: 20, height: 20, borderRadius: 6 }} /></span>
      </header>
      <nav className="side" aria-hidden="true">
        <div className="brand">
          <span className="sk-brand-logo"><Icon.Brand size={30} /></span>
          <span className="brand-name">{title}</span>
        </div>
        <div className="side-menu">
          {[72, 58, 66, 52, 70, 60, 64, 56].map((w, i) => (
            <div key={i} className="sk-nav-item">
              <span className="skeleton sk-nav-ico" />
              <span className="skeleton" style={{ width: `${w}%`, height: 13 }} />
            </div>
          ))}
        </div>
        <div className="side-foot">
          <div className="sk-nav-item">
            <span className="skeleton" style={{ width: 32, height: 32, borderRadius: '50%' }} />
            <span style={{ flex: 1, display: 'flex', flexDirection: 'column', gap: 6 }}>
              <span className="skeleton" style={{ width: '70%', height: 12 }} />
              <span className="skeleton" style={{ width: '45%', height: 10 }} />
            </span>
          </div>
        </div>
      </nav>
      <main className="main">
        <header className="workspace-bar" aria-hidden="true">
          <span className="skeleton" style={{ width: 180, height: 14 }} />
          <span className="skeleton" style={{ width: 250, height: 32, borderRadius: 6 }} />
        </header>
        <div className="page-content">
          <div className="top">
            <span className="skeleton" style={{ width: 200, height: 25 }} />
            <span className="skeleton" style={{ width: 120, height: 36, borderRadius: 8 }} />
          </div>
          <div className="sk-loading"><span className="spin" aria-hidden="true" />{t('加载中…')}</div>
          <SkeletonRows rows={6} cols={4} />
        </div>
      </main>
    </div>
  )
}

// PageSkeleton is the Suspense fallback for lazily loaded pages: a title
// bar and shimmer table rows inside the already-rendered shell, instead
// of a bare "加载中…" line.
function PageSkeleton() {
  return (
    <div aria-busy="true" aria-label={t('加载中')}>
      <div className="top" style={{ marginBottom: 24 }}>
        <span className="skeleton" style={{ width: 180, height: 25 }} />
        <span className="skeleton" style={{ width: 110, height: 36, borderRadius: 8 }} />
      </div>
      <div className="sk-loading"><span className="spin" aria-hidden="true" />{t('加载中…')}</div>
      <SkeletonRows rows={6} cols={4} />
    </div>
  )
}

function Layout({ user, avatarURL, onLogout, children }: { user: string; avatarURL?: string; onLogout: () => Promise<void>; children: ReactNode }) {
  useMobileKeyboard()
  useMenuDismiss()
  const leave = useSettingsLeave()
  const { title } = useBrand()
  const [menu, setMenu] = useState(false) // the sidebar, as a drawer on phones
  // On wide screens the sidebar folds to an icon rail; remembered per browser.
  const [folded, setFolded] = useState(() => { try { return localStorage.getItem('polaris.sidebar') === 'folded' } catch { return false } })
  const fold = (v: boolean) => { setFolded(v); try { localStorage.setItem('polaris.sidebar', v ? 'folded' : 'open') } catch { /* private mode */ } }
  useEffect(() => { document.title = title }, [title])
  const themesOK = useFeature('themes')
  useEffect(() => { setPremiumThemes(themesOK) }, [themesOK])
  const loc = useLocation()
  useEffect(() => { setMenu(false) }, [loc.pathname])
  useEffect(() => {
    if (!menu) return
    const esc = (e: KeyboardEvent) => { if (e.key === 'Escape') setMenu(false) }
    document.addEventListener('keydown', esc)
    document.body.classList.add('noscroll')
    return () => { document.removeEventListener('keydown', esc); document.body.classList.remove('noscroll') }
  }, [menu])
  const { scoped } = useScope()
  const accountName = user.replace(/^tg:/, '')
  const accountRole = scoped ? t('节点管理员') : user.startsWith('tg:') ? t('Telegram 登录') : t('管理员')
  const { nodes } = useNodes()
  const [palette, setPalette] = useState(false)
  useEffect(() => {
    const k = (e: KeyboardEvent) => { if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'k') { e.preventDefault(); setPalette((x) => !x) } }
    document.addEventListener('keydown', k)
    return () => document.removeEventListener('keydown', k)
  }, [])
  useLayoutEffect(() => { setPalette(false) }, [loc.pathname, loc.search])
  // Installed plugins add their admin pages under 扩展.
  const plugins = usePlugins(!scoped)
  const items = nav.map((n) => {
    const pg = (plugins?.plugins ?? []).filter((p) => p.enabled).flatMap((p) => (p.manifest.pages ?? []).map((x) => ({ to: `/plugins/${p.id}/${x.id}`, label: x.title, icon: <Icon.Extension /> })))
    return n.to === '/extensions' && pg.length > 0 ? { ...n, children: [{ to: '/extensions', label: t('扩展管理'), icon: <Icon.Extension /> }, ...pg] } : n
  })
  const pages: PaletteItem[] = scoped
    ? [{ id: 'p/nodes', label: t('我的节点'), icon: <Icon.Server />, to: '/nodes', group: t('页面') }]
    : items.flatMap((n) => n.children ?? [n]).map((n) => ({ id: 'p' + n.to, label: n.label, icon: n.icon, to: n.to, group: t('页面') }))
  const isMac = /Mac|iPhone|iPad/.test(navigator.platform)
  const inTabs = tabs.some((tb) => (tb.end ? loc.pathname === tb.to : loc.pathname.startsWith(tb.to)))
  const currentPage = pages.find((p) => p.to === '/' ? loc.pathname === '/' : loc.pathname === p.to || loc.pathname.startsWith(p.to + '/'))
  const currentSection = items.find((item) => item.children?.some((child) => loc.pathname === child.to || loc.pathname.startsWith(child.to + '/')))
  return (
    <div className={'app' + (folded ? ' folded' : '')}>
      <TopProgress />
      <header className="mtop">
        <button type="button" className="mtop-btn" aria-label={t('打开菜单')} aria-expanded={menu} onClick={() => setMenu(true)}><Icon.Menu /></button>
        <div className="mtop-title"><Icon.Brand size={26} /><span>{title}</span></div>
        <button type="button" className="mtop-btn" aria-label={t('搜索')} onClick={() => setPalette(true)}><Icon.Search /></button>
        <ThemeToggle themesOK={themesOK} />
      </header>
      <div className={'side-mask' + (menu ? ' on' : '')} onClick={() => setMenu(false)} aria-hidden="true" />
      <nav id="console-sidebar" className={'side' + (menu ? ' open' : '')} aria-label={t('主导航')}>
        <div className="brand"><Icon.Brand size={30} /><span className="brand-name">{title}</span><button type="button" className="side-close" aria-label={t('关闭菜单')} onClick={() => setMenu(false)}><Icon.Close /></button></div>
        <div className="side-menu">
          {scoped ? (
            <>
              <NavLink to="/nodes" className="nav" title={folded ? t('我的节点') : undefined}><Icon.Server />{t('我的节点')}</NavLink>
              <div className="navsec">{t('仅可操作分配给你的节点')}</div>
              {!user.startsWith('tg:') && <NavLink to="/account" className="nav"><Icon.Lock />{t('我的账号')}</NavLink>}
            </>
          ) : (
            <>
              {items.map((n) => n.children ? <NavGroup key={n.label} item={n} folded={folded} onUnfold={() => fold(false)} /> : <NavLink key={n.to} to={n.to} end={n.end} className="nav" title={folded ? n.label : undefined}>{n.icon}{n.label}</NavLink>)}
            </>
          )}
        </div>
        <SidebarAccount name={accountName} role={accountRole} avatarURL={avatarURL} folded={folded}
          securityURL={scoped ? user.startsWith('tg:') ? undefined : '/account' : '/settings#security'}
          preferences={<><LangToggle /><ThemeToggle themesOK={themesOK} /></>}
          onLogout={async () => { if (await leave()) await onLogout() }} />
      </nav>
      <main className="main">
        <header className="workspace-bar">
          <button type="button" className="side-fold" aria-label={folded ? t('展开侧栏') : t('收起侧栏')} aria-pressed={folded} aria-controls="console-sidebar" title={folded ? t('展开侧栏') : t('收起侧栏')} onClick={() => fold(!folded)}><Icon.Fold /></button>
          <nav className="workspace-breadcrumb" aria-label={t('当前位置')}>
            <Link to="/"><span>{t('控制台')}</span></Link><span aria-hidden="true">/</span>
            {currentSection && currentSection.label !== currentPage?.label && <><span className="workspace-section">{currentSection.label}</span><span aria-hidden="true">/</span></>}
            <b aria-current="page">{currentPage?.label ?? title}</b>
          </nav>
          <button type="button" className="workspace-search" aria-label={t('搜索')} onClick={() => setPalette(true)}><Icon.Search /><span>{scoped ? t('搜索页面、节点…') : t('搜索页面、节点、用户…')}</span><kbd>{isMac ? '⌘' : 'Ctrl'} K</kbd></button>
        </header>
        <div className="page-content" key={loc.pathname}><LoadFailures /><PageBoundary>{children}</PageBoundary></div>
      </main>
      <CommandPalette open={palette} onClose={() => setPalette(false)} pages={pages} nodes={nodes} searchUsers={!scoped} />
      {!scoped && <nav className="mtabs" aria-label={t('快捷导航')}>
        {tabs.map((tb) => <NavLink key={tb.to} to={tb.to} end={tb.end} className="mtab">{tb.icon}<span>{tb.label}</span></NavLink>)}
        <button type="button" className={'mtab' + (!inTabs || menu ? ' active' : '')} onClick={() => setMenu(true)}><Icon.More /><span>{t('更多')}</span></button>
      </nav>}
    </div>
  )
}

function Session() {
  // The English dictionary loads in two phases: the shell dictionaries
  // before first paint, the rest in the background (see i18n.ts). When
  // the rest arrives, re-render once so any label that briefly fell back
  // to Chinese shows its English translation.
  const [, setDictTick] = useState(0)
  useEffect(() => {
    loadRestInBackground()
    return onDictReady(() => setDictTick((x) => x + 1))
  }, [])
  const [user, setUser] = useState<string | null>(() => {
    const r = takeOAuthResult() // GitHub sign-in lands here with a session token
    if (r.token) { setToken(r.token); return r.user ?? '…' }
    if (r.tg) return null // Telegram sign-in link: exchanged below
    return getToken() ? '…' : null
  })
  const [scoped, setScoped] = useState(false)
  const [role, setRole] = useState<string | null>(null)
  const [avatar, setAvatar] = useState({ username: '', url: '' })
  const avatarURL = avatar.username === user ? avatar.url : ''
  const [accessError, setAccessError] = useState('')
  const [tgBusy, setTgBusy] = useState(() => !!takeOAuthResult().tg)
  useEffect(() => {
    setUnauthorizedHandler(() => { setToken(null); setUser(null); setScoped(false); setRole(null); setAvatar({ username: '', url: '' }) })
    const r = takeOAuthResult()
    if (r.tg) {
      setToken(null)
      post<{ token: string; username: string }>('/auth/tg', { code: r.tg })
        .then((x) => { setToken(x.token); setUser(x.username) })
        .catch((e) => { r.error = e instanceof Error ? e.message : String(e) })
        .finally(() => setTgBusy(false))
    }
  }, [])
  useEffect(() => {
    if (!user || !getToken()) return
    get<{ username: string; role?: string; nodeIds?: string[]; avatarURL?: string }>('/auth/me').then((m) => { setUser(m.username); setAvatar({ username: m.username, url: m.avatarURL || '' }); setRole(m.role || 'owner'); setScoped(m.role === 'operator' || !!m.nodeIds); setAccessError('') }).catch((e) => setAccessError(String(e)))
  }, [user === null])
  useEffect(() => {
    const refreshAvatar = () => { get<{ username: string; avatarURL?: string }>('/auth/me').then((m) => setAvatar({ username: m.username, url: m.avatarURL || '' })).catch(() => {}) }
    window.addEventListener('polaris-avatar-updated', refreshAvatar)
    return () => window.removeEventListener('polaris-avatar-updated', refreshAvatar)
  }, [])
  const logout = () => post('/auth/logout').catch(() => {}).finally(() => { setToken(null); setUser(null); setScoped(false); setRole(null); setAvatar({ username: '', url: '' }) })
  if (tgBusy) return <div className="login"><div className="card" style={{ alignItems: 'center' }}>{t('正在通过 Telegram 登录…')}</div></div>
  return (
    <ToastProvider>
      <ConfirmProvider>
        <SudoPrompt />
        <SettingsDraftProvider>
          {!user ? (
            <Login onLogin={(u) => { setRole(null); setAccessError(''); setUser(u) }} />
          ) : role === null ? (
            accessError ? (
              <div className="card" style={{ margin: 20 }}><p role="alert">{t('无法读取账号权限')}</p><button className="btn" onClick={() => location.reload()}>{t('重新加载')}</button></div>
            ) : <AppShellSkeleton />
          ) : !['owner', 'operator', 'viewer', 'support', 'finance'].includes(role) ? (
            <div className="card" style={{ margin: 20 }}><p role="alert">{t('无法读取账号权限')}</p><button className="btn" onClick={logout}>{t('退出登录')}</button></div>
          ) : ['viewer', 'support', 'finance'].includes(role) ? (
            <StaffWorkspace role={role} username={user} avatarURL={avatarURL} onLogout={logout} />
          ) : (
            <ScopeCtx.Provider value={{ scoped }}>
              <NodesProvider>
                <Layout user={user} avatarURL={avatarURL} onLogout={logout}>
                  <Suspense fallback={<PageSkeleton />}>
                  {scoped ? (
                    <Routes>
                      <Route path="/nodes" element={<Nodes />} />
                      <Route path="/account" element={<StaffAccount />} />
                      <Route path="*" element={<Navigate to="/nodes" replace />} />
                    </Routes>
                  ) : (
                    <Routes>
                      {routeLeaves.map((r) => {
                        const El = r.element
                        return <Route key={r.to} path={r.to} element={El ? <El /> : undefined} />
                      })}
                      <Route path="/plugins/:id/:page" element={<PluginPage />} />
                      <Route path="*" element={<Navigate to="/" replace />} />
                    </Routes>
                  )}
                  </Suspense>
                </Layout>
              </NodesProvider>
            </ScopeCtx.Provider>
          )}
        </SettingsDraftProvider>
      </ConfirmProvider>
    </ToastProvider>
  )
}

// A data router lets unsaved settings block links, command navigation and Back.
// Consume OAuth fragments before the router takes its initial location snapshot.
takeOAuthResult()
const router = createBrowserRouter([{ path: '*', element: <Session /> }], { basename: BASE.replace(/\/$/, '') || '/' })
export default function App() { return <RouterProvider router={router} /> }
