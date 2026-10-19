import { Component, createContext, useCallback, useContext, useEffect, useState, type ReactNode } from 'react'
import { BrowserRouter, NavLink, Navigate, Route, Routes, useLocation } from 'react-router-dom'
import { BASE, get, getToken, onInflight, post, setToken, setUnauthorizedHandler, takeOAuthResult, wsURL } from './api'
import { Icon } from './components/Icons'
import CommandPalette, { type PaletteItem } from './components/CommandPalette'
import { recordRates } from './components/NodeCard'
import { setPremiumThemes, useTheme } from './theme'
import { usePlugins } from './plugins'
import { useBrand } from './brand'
import { useFeature } from './components/License'
import { t } from './i18n'
import LangToggle from './components/LangToggle'
import { ConfirmProvider, ToastProvider } from './components/ui'
import SudoPrompt from './components/SudoPrompt'
import type { Node } from './types'
import Login from './pages/Login'
import PluginPage from './pages/PluginPage'
import Dashboard from './pages/Dashboard'
import Stats from './pages/Stats'
import Nodes from './pages/Nodes'
import NodeMap from './pages/NodeMap'
import Cores from './pages/Cores'
import Forwards from './pages/Forwards'
import Inbounds from './pages/Inbounds'
import Chains from './pages/Chains'
import Templates from './pages/Templates'
import Users from './pages/Users'
import Shop from './pages/Shop'
import Plans from './pages/Plans'
import SubTemplates from './pages/SubTemplates'
import Certificates from './pages/Certificates'
import Settings from './pages/Settings'
import Audit from './pages/Audit'
import Convert from './pages/Convert'
import Domains from './pages/Domains'
import Telegram from './pages/Telegram'
import Costs from './pages/Costs'
import LicensePage from './pages/LicensePage'
import Extensions from './pages/Extensions'

// Scope of the session: Telegram node operators see only their own nodes.
const ScopeCtx = createContext<{ scoped: boolean }>({ scoped: false })
export function useScope() { return useContext(ScopeCtx) }

// Live node list shared by pages, fed by the WebSocket.
const NodesCtx = createContext<{ nodes: Node[]; refresh: () => void }>({ nodes: [], refresh: () => {} })
export function useNodes() { return useContext(NodesCtx) }

function NodesProvider({ children }: { children: ReactNode }) {
  const [nodes, setNodes] = useState<Node[]>([])
  const refresh = useCallback(() => { get<Node[]>('/nodes').then(setNodes).catch(() => {}) }, [])
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
          if (m.type === 'nodes') setNodes(m.data)
        } catch { /* ignore */ }
      }
      ws.onclose = () => { if (!stop) retry = setTimeout(connect, 5000) }
    }
    connect()
    return () => { stop = true; clearTimeout(retry); ws?.close() }
  }, [refresh])
  useEffect(() => { recordRates(nodes) }, [nodes])
  return <NodesCtx.Provider value={{ nodes, refresh }}>{children}</NodesCtx.Provider>
}

type NavItem = { to: string; label: string; icon: ReactNode; end?: boolean; children?: { to: string; label: string; icon: ReactNode }[] }

// Sidebar: related pages share one collapsible group so the list stays short.
const nav: NavItem[] = [
  { to: '/', label: t('仪表盘'), icon: <Icon.Dashboard />, end: true },
  { to: '/stats', label: t('统计'), icon: <Icon.Chart /> },
  { to: '/nodes', label: t('节点'), icon: <Icon.Nodes />, children: [
    { to: '/nodes', label: t('节点列表'), icon: <Icon.Server /> },
    { to: '/map', label: t('节点地图'), icon: <Icon.Map /> },
    { to: '/cores', label: t('内核'), icon: <Icon.Cpu /> },
    { to: '/costs', label: t('服务器成本'), icon: <Icon.Wallet /> },
  ] },
  { to: '/inbounds', label: t('入站与转发'), icon: <Icon.Inbound />, children: [
    { to: '/inbounds', label: t('入站'), icon: <Icon.Inbound /> },
    { to: '/templates', label: t('入站模板'), icon: <Icon.Template /> },
    { to: '/forwards', label: t('端口转发'), icon: <Icon.Forward /> },
  ] },
  { to: '/chains', label: t('链路与路由'), icon: <Icon.Chain /> },
  { to: '/users', label: t('用户'), icon: <Icon.Users />, children: [
    { to: '/users', label: t('用户'), icon: <Icon.Users /> },
    { to: '/plans', label: t('套餐'), icon: <Icon.Package /> },
    { to: '/shop', label: t('售卖'), icon: <Icon.Wallet /> },
  ] },
  { to: '/sub-templates', label: t('订阅'), icon: <Icon.Sub />, children: [
    { to: '/sub-templates', label: t('订阅设置'), icon: <Icon.Sub /> },
    { to: '/convert', label: t('订阅转换'), icon: <Icon.Convert /> },
  ] },
  { to: '/domains', label: t('域名与证书'), icon: <Icon.Globe />, children: [
    { to: '/domains', label: t('域名与 DDNS'), icon: <Icon.Globe /> },
    { to: '/certificates', label: t('证书'), icon: <Icon.Lock /> },
  ] },
  { to: '/extensions', label: t('扩展'), icon: <Icon.Extension /> },
  { to: '/settings', label: t('系统'), icon: <Icon.Gear />, children: [
    { to: '/settings', label: t('设置'), icon: <Icon.Gear /> },
    { to: '/telegram', label: t('Telegram 机器人'), icon: <Icon.Bot /> },
    { to: '/audit', label: t('审计日志'), icon: <Icon.Log /> },
  ] },
  { to: '/license', label: t('许可证'), icon: <Icon.Lock /> },
]

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
  const [open, setOpen] = useState(here)
  useEffect(() => { if (here) setOpen(true) }, [here])
  return (
    <div className={'navgroup' + (open ? ' open' : '')}>
      <button type="button" className={'nav' + (here && !open ? ' active' : '')} aria-expanded={open} title={folded ? item.label : undefined} onClick={() => { if (folded) { setOpen(true); onUnfold() } else setOpen(!open) }}>
        {item.icon}{item.label}<span className="chev" aria-hidden="true"><Icon.Chevron /></span>
      </button>
      <div className="navsub">
        {item.children!.map((c) => <NavLink key={c.to} to={c.to} className="nav" title={folded ? c.label : undefined}>{c.icon}{c.label}</NavLink>)}
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

function Layout({ user, onLogout, children }: { user: string; onLogout: () => void; children: ReactNode }) {
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
  useEffect(() => { setPalette(false) }, [loc.pathname, loc.search])
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
      <nav className={'side' + (menu ? ' open' : '')} aria-label={t('主导航')}>
        <div className="brand"><Icon.Brand size={30} /><span className="brand-name">{title}</span><button type="button" className="side-fold" aria-label={folded ? t('展开侧栏') : t('收起侧栏')} aria-pressed={folded} title={folded ? t('展开侧栏') : t('收起侧栏')} onClick={() => fold(!folded)}><Icon.Fold /></button><button type="button" className="side-close" aria-label={t('关闭菜单')} onClick={() => setMenu(false)}><Icon.Close /></button></div>
        <button type="button" className="side-search" title={folded ? t('搜索') : undefined} onClick={() => setPalette(true)}><Icon.Search /><span>{t('搜索')}</span><kbd>{isMac ? '⌘' : 'Ctrl'} K</kbd></button>
        {scoped ? (
          <>
            <NavLink to="/nodes" className="nav" title={folded ? t('我的节点') : undefined}><Icon.Server />{t('我的节点')}</NavLink>
            <div className="navsec">{t('Telegram 节点管理员 · 仅可查看分配给你的节点')}</div>
          </>
        ) : (
          <>
            {items.map((n) => n.children ? <NavGroup key={n.label} item={n} folded={folded} onUnfold={() => fold(false)} /> : <NavLink key={n.to} to={n.to} end={n.end} className="nav" title={folded ? n.label : undefined}>{n.icon}{n.label}</NavLink>)}
          </>
        )}
        <div className="side-foot">
          <span className="me"><span className="avatar" title={accountName}>{(accountName[0] || '?').toUpperCase()}</span><span className="me-text"><b title={accountName}>{accountName}</b><small title={accountRole}>{accountRole}</small></span></span>
          <span className="side-actions"><LangToggle /><ThemeToggle themesOK={themesOK} /><button type="button" className="logout" title={t('退出登录')} aria-label={t('退出登录')} onClick={onLogout}>{t('退出')}</button></span>
        </div>
      </nav>
      <main className="main" key={loc.pathname}><PageBoundary>{children}</PageBoundary></main>
      <CommandPalette open={palette} onClose={() => setPalette(false)} pages={pages} nodes={nodes} searchUsers={!scoped} />
      {!scoped && <nav className="mtabs" aria-label={t('快捷导航')}>
        {tabs.map((tb) => <NavLink key={tb.to} to={tb.to} end={tb.end} className="mtab">{tb.icon}<span>{tb.label}</span></NavLink>)}
        <button type="button" className={'mtab' + (!inTabs || menu ? ' active' : '')} onClick={() => setMenu(true)}><Icon.More /><span>{t('更多')}</span></button>
      </nav>}
    </div>
  )
}

export default function App() {
  const [user, setUser] = useState<string | null>(() => {
    const r = takeOAuthResult() // GitHub sign-in lands here with a session token
    if (r.token) { setToken(r.token); return r.user ?? '…' }
    if (r.tg) return null // Telegram sign-in link: exchanged below
    return getToken() ? '…' : null
  })
  const [scoped, setScoped] = useState(false)
  const [tgBusy, setTgBusy] = useState(() => !!takeOAuthResult().tg)
  useEffect(() => {
    setUnauthorizedHandler(() => { setToken(null); setUser(null); setScoped(false) })
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
    get<{ username: string; nodeIds?: string[] }>('/auth/me').then((m) => { setUser(m.username); setScoped(!!m.nodeIds) }).catch(() => {})
  }, [user === null])
  const logout = () => { post('/auth/logout').catch(() => {}).finally(() => { setToken(null); setUser(null); setScoped(false) }) }
  if (tgBusy) return <div className="login"><div className="card" style={{ alignItems: 'center' }}>{t('正在通过 Telegram 登录…')}</div></div>
  return (
    <ToastProvider>
      <ConfirmProvider>
        <SudoPrompt />
        <BrowserRouter basename={BASE.replace(/\/$/, '') || '/'}>
          {!user ? (
            <Login onLogin={(u) => setUser(u)} />
          ) : (
            <ScopeCtx.Provider value={{ scoped }}>
              <NodesProvider>
                <Layout user={user} onLogout={logout}>
                  {scoped ? (
                    <Routes>
                      <Route path="/nodes" element={<Nodes />} />
                      <Route path="*" element={<Navigate to="/nodes" replace />} />
                    </Routes>
                  ) : (
                    <Routes>
                      <Route path="/" element={<Dashboard />} />
                      <Route path="/stats" element={<Stats />} />
                      <Route path="/nodes" element={<Nodes />} />
                      <Route path="/map" element={<NodeMap />} />
                      <Route path="/cores" element={<Cores />} />
                      <Route path="/forwards" element={<Forwards />} />
                      <Route path="/inbounds" element={<Inbounds />} />
                      <Route path="/chains" element={<Chains />} />
                      <Route path="/templates" element={<Templates />} />
                      <Route path="/users" element={<Users />} />
                      <Route path="/shop" element={<Shop />} />
                      <Route path="/plans" element={<Plans />} />
                      <Route path="/sub-templates" element={<SubTemplates />} />
                      <Route path="/certificates" element={<Certificates />} />
                      <Route path="/settings" element={<Settings />} />
                      <Route path="/audit" element={<Audit />} />
                      <Route path="/convert" element={<Convert />} />
                      <Route path="/domains" element={<Domains />} />
                      <Route path="/telegram" element={<Telegram />} />
                      <Route path="/costs" element={<Costs />} />
                      <Route path="/extensions" element={<Extensions />} />
                      <Route path="/plugins/:id/:page" element={<PluginPage />} />
                      <Route path="/license" element={<LicensePage />} />
                      <Route path="*" element={<Navigate to="/" replace />} />
                    </Routes>
                  )}
                </Layout>
              </NodesProvider>
            </ScopeCtx.Provider>
          )}
        </BrowserRouter>
      </ConfirmProvider>
    </ToastProvider>
  )
}
