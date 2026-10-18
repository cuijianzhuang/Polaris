// Stroke icons drawn to match the design canvas (18px, 1.75 stroke).
import { useState, type ReactNode } from 'react'
import polarisIcon from '../assets/polaris-icon.png'
import { logoURL, useBrand } from '../brand'

function Svg({ children, size = 18 }: { children: ReactNode; size?: number }) {
  return (
    <svg width={size} height={size} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.75"
      strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">{children}</svg>
  )
}

export const Icon = {
  Logo: () => <LogoTile size={24} />,
  LogoTile: ({ size = 40 }: { size?: number }) => <LogoTile size={size} />,
  // Brand: the custom logo when one is set (paid feature), else the tile.
  Brand: ({ size = 40 }: { size?: number }) => <BrandLogo size={size} />,
  GitHub: () => (
    <svg width="18" height="18" viewBox="0 0 16 16" fill="currentColor" aria-hidden="true">
      <path d="M8 0C3.58 0 0 3.58 0 8c0 3.54 2.29 6.53 5.47 7.59.4.07.55-.17.55-.38 0-.19-.01-.82-.01-1.49-2.01.37-2.53-.49-2.69-.94-.09-.23-.48-.94-.82-1.13-.28-.15-.68-.52-.01-.53.63-.01 1.08.58 1.23.82.72 1.21 1.87.87 2.33.66.07-.52.28-.87.51-1.07-1.78-.2-3.64-.89-3.64-3.95 0-.87.31-1.59.82-2.15-.08-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82.64-.18 1.32-.27 2-.27.68 0 1.36.09 2 .27 1.53-1.04 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.51.56.82 1.27.82 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73.54 1.48 0 1.07-.01 1.93-.01 2.2 0 .21.15.46.55.38A8.013 8.013 0 0016 8c0-4.42-3.58-8-8-8z" />
    </svg>
  ),
  Telegram: () => (
    <svg width="18" height="18" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true">
      <path d="M21.94 4.3a1.1 1.1 0 0 0-1.5-1.2L2.7 10.1c-.97.39-.95 1.77.03 2.13l4.43 1.6 1.7 5.47c.2.66 1.03.87 1.52.38l2.5-2.45 4.62 3.4c.63.47 1.53.12 1.7-.65L21.94 4.3ZM9.5 14.3l-.5 3.2-1.15-3.9 9.65-6.1-8 6.8Z" />
    </svg>
  ),
  Search: () => <Svg><circle cx="11" cy="11" r="7" /><path d="m20 20-3.5-3.5" /></Svg>,
  Dashboard: () => <Svg><rect x="3" y="3" width="7" height="9" rx="1" /><rect x="14" y="3" width="7" height="5" rx="1" /><rect x="14" y="12" width="7" height="9" rx="1" /><rect x="3" y="16" width="7" height="5" rx="1" /></Svg>,
  Chart: () => <Svg><path d="M4 20V10M10 20V4M16 20v-7M22 20H2" /></Svg>,
  Nodes: () => <Svg><rect x="3" y="4" width="18" height="6" rx="1" /><rect x="3" y="14" width="18" height="6" rx="1" /></Svg>,
  Inbound: () => <Svg><path d="M4 12h12M12 6l6 6-6 6" /></Svg>,
  Chain: () => <Svg><path d="M10 14a4 4 0 005.7 0l3-3a4 4 0 00-5.7-5.7l-1 1M14 10a4 4 0 00-5.7 0l-3 3a4 4 0 005.7 5.7l1-1" /></Svg>,
  Template: () => <Svg><path d="M12 3l9 5-9 5-9-5z" /><path d="M3 13l9 5 9-5" /></Svg>,
  Users: () => <Svg><circle cx="9" cy="8" r="4" /><path d="M2 21c1-4 4-6 7-6s6 2 7 6M17 11a3 3 0 100-6M22 20c-.5-2.5-2-4-4-4.6" /></Svg>,
  Sub: () => <Svg><path d="M4 6h16M4 12h16M4 18h10" /></Svg>,
  Lock: () => <Svg><rect x="5" y="11" width="14" height="10" rx="2" /><path d="M8 11V7a4 4 0 018 0v4" /></Svg>,
  Gear: () => <Svg><path d="M12.22 2h-.44a2 2 0 0 0-2 2v.18a2 2 0 0 1-1 1.73l-.43.25a2 2 0 0 1-2 0l-.15-.08a2 2 0 0 0-2.73.73l-.22.38a2 2 0 0 0 .73 2.73l.15.1a2 2 0 0 1 1 1.72v.51a2 2 0 0 1-1 1.74l-.15.09a2 2 0 0 0-.73 2.73l.22.38a2 2 0 0 0 2.73.73l.15-.08a2 2 0 0 1 2 0l.43.25a2 2 0 0 1 1 1.73V20a2 2 0 0 0 2 2h.44a2 2 0 0 0 2-2v-.18a2 2 0 0 1 1-1.73l.43-.25a2 2 0 0 1 2 0l.15.08a2 2 0 0 0 2.73-.73l.22-.39a2 2 0 0 0-.73-2.73l-.15-.08a2 2 0 0 1-1-1.74v-.5a2 2 0 0 1 1-1.74l.15-.09a2 2 0 0 0 .73-2.73l-.22-.38a2 2 0 0 0-2.73-.73l-.15.08a2 2 0 0 1-2 0l-.43-.25a2 2 0 0 1-1-1.73V4a2 2 0 0 0-2-2z" /><circle cx="12" cy="12" r="3" /></Svg>,
  Forward: () => <Svg><path d="M4 7h11M11 3l4 4-4 4M20 17H9M13 13l-4 4 4 4" /></Svg>,
  Download: () => <Svg><path d="M12 4v11M7 10l5 5 5-5M5 20h14" /></Svg>,
  Reset: () => <Svg><path d="M4 12a8 8 0 1 0 2.3-5.6M4 4v4h4" /></Svg>,
  Menu: () => <Svg size={20}><path d="M4 7h16M4 12h16M4 17h16" /></Svg>,
  More: () => <Svg><circle cx="5" cy="12" r="1.2" /><circle cx="12" cy="12" r="1.2" /><circle cx="19" cy="12" r="1.2" /></Svg>,
  Chevron: () => <Svg size={14}><path d="M9 6l6 6-6 6" /></Svg>,
  Map: () => <Svg><path d="M9 4L3 6.5v13.5l6-2.5 6 2.5 6-2.5V4l-6 2.5L9 4z" /><path d="M9 4v13.5M15 6.5V20" /></Svg>,
  Server: () => <Svg><rect x="3" y="4" width="18" height="7" rx="1.5" /><rect x="3" y="13" width="18" height="7" rx="1.5" /><path d="M7 7.5h.01M7 16.5h.01" /></Svg>,
  Cpu: () => <Svg><rect x="6" y="6" width="12" height="12" rx="2" /><rect x="9.5" y="9.5" width="5" height="5" rx=".5" /><path d="M9 2v3M15 2v3M9 19v3M15 19v3M2 9h3M2 15h3M19 9h3M19 15h3" /></Svg>,
  Sun: () => <Svg><circle cx="12" cy="12" r="4" /><path d="M12 2v2M12 20v2M4.93 4.93l1.41 1.41M17.66 17.66l1.41 1.41M2 12h2M20 12h2M6.34 17.66l-1.41 1.41M19.07 4.93l-1.41 1.41" /></Svg>,
  Moon: () => <Svg><path d="M21 12.79A9 9 0 1 1 11.21 3 7 7 0 0 0 21 12.79z" /></Svg>,
  Monitor: () => <Svg><rect x="2" y="3" width="20" height="14" rx="2" /><path d="M8 21h8M12 17v4" /></Svg>,
  Log: () => <Svg><path d="M6 3h9l4 4v14H6z" /><path d="M9 12h7M9 16h7" /></Svg>,
  Copy: () => <Svg><rect x="9" y="9" width="11" height="11" rx="2" /><path d="M5 15V5a1 1 0 011-1h10" /></Svg>,
  QR: () => <Svg><rect x="3" y="3" width="7" height="7" /><rect x="14" y="3" width="7" height="7" /><rect x="3" y="14" width="7" height="7" /><path d="M14 14h3v3M21 14v7h-7" /></Svg>,
  Edit: () => <Svg><path d="M4 20h4L19 9l-4-4L4 16z" /></Svg>,
  Trash: () => <Svg><path d="M4 7h16M10 11v6M14 11v6M6 7l1 13h10l1-13M9 7V4h6v3" /></Svg>,
  Fold: () => <Svg><rect x="3.5" y="4.5" width="17" height="15" rx="2.5" /><path d="M9.5 4.5v15" /></Svg>,
  Close: () => <Svg><path d="M6 6l12 12M18 6L6 18" /></Svg>,
  Plus: () => <Svg><path d="M12 5v14M5 12h14" /></Svg>,
  Refresh: () => <Svg><path d="M20 12a8 8 0 11-2.3-5.6M20 4v5h-5" /></Svg>,
  Eye: () => <Svg><path d="M2 12s4-7 10-7 10 7 10 7-4 7-10 7S2 12 2 12z" /><circle cx="12" cy="12" r="3" /></Svg>,
  Globe: () => <Svg><circle cx="12" cy="12" r="9" /><path d="M3 12h18M12 3a14 14 0 010 18M12 3a14 14 0 000 18" /></Svg>,
  Convert: () => <Svg><path d="M4 7h13l-3-3M20 17H7l3 3" /></Svg>,
  Extension: () => <Svg><path d="M4 5h5V4a2 2 0 0 1 4 0v1h6v5h1a2 2 0 0 1 0 4h-1v5h-5v-1a2 2 0 0 0-4 0v1H4v-5h1a2 2 0 0 0 0-4H4z" /></Svg>,
  Wallet: () => <Svg><rect x="3" y="6" width="18" height="14" rx="2" /><path d="M3 10h18M16 15h2" /><path d="M6 6V5a1 1 0 011-1h10a1 1 0 011 1v1" /></Svg>,
  Bot: () => <Svg><rect x="4" y="8" width="16" height="11" rx="3" /><path d="M12 4v4M9 13h.01M15 13h.01" /></Svg>,
}

// The built-in mark shares the same fingerprinted artwork as the favicon.
function LogoTile({ size }: { size: number }) {
  return <img src={polarisIcon} width={size} height={size} alt="" aria-hidden="true" style={{ display: 'block', flexShrink: 0 }} />
}

function BrandLogo({ size }: { size: number }) {
  const { logo } = useBrand()
  const [broken, setBroken] = useState('')
  if (!logo || broken === logo) return <LogoTile size={size} />
  return <img className="brand-logo" src={logoURL(logo)} width={size} height={size} alt="" onError={() => setBroken(logo)} />
}
