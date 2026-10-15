// Stroke icons drawn to match the design canvas (18px, 1.75 stroke).
import type { ReactNode } from 'react'

function Svg({ children, size = 18 }: { children: ReactNode; size?: number }) {
  return (
    <svg width={size} height={size} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.75"
      strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">{children}</svg>
  )
}

export const Icon = {
  Logo: () => (
    <svg width="22" height="22" viewBox="0 0 24 24" fill="none" stroke="#5CC8DB" strokeWidth="2" aria-hidden="true">
      <path d="M12 2v20M2 12h20M5 5l14 14M19 5L5 19" />
    </svg>
  ),
  Dashboard: () => <Svg><rect x="3" y="3" width="7" height="9" rx="1" /><rect x="14" y="3" width="7" height="5" rx="1" /><rect x="14" y="12" width="7" height="9" rx="1" /><rect x="3" y="16" width="7" height="5" rx="1" /></Svg>,
  Nodes: () => <Svg><rect x="3" y="4" width="18" height="6" rx="1" /><rect x="3" y="14" width="18" height="6" rx="1" /></Svg>,
  Inbound: () => <Svg><path d="M4 12h12M12 6l6 6-6 6" /></Svg>,
  Chain: () => <Svg><path d="M10 14a4 4 0 005.7 0l3-3a4 4 0 00-5.7-5.7l-1 1M14 10a4 4 0 00-5.7 0l-3 3a4 4 0 005.7 5.7l1-1" /></Svg>,
  Template: () => <Svg><path d="M12 3l9 5-9 5-9-5z" /><path d="M3 13l9 5 9-5" /></Svg>,
  Users: () => <Svg><circle cx="9" cy="8" r="4" /><path d="M2 21c1-4 4-6 7-6s6 2 7 6M17 11a3 3 0 100-6M22 20c-.5-2.5-2-4-4-4.6" /></Svg>,
  Sub: () => <Svg><path d="M4 6h16M4 12h16M4 18h10" /></Svg>,
  Lock: () => <Svg><rect x="5" y="11" width="14" height="10" rx="2" /><path d="M8 11V7a4 4 0 018 0v4" /></Svg>,
  Gear: () => <Svg><circle cx="12" cy="12" r="3" /><path d="M12 2v3M12 19v3M2 12h3M19 12h3M5 5l2 2M17 17l2 2M19 5l-2 2M7 17l-2 2" /></Svg>,
  Log: () => <Svg><path d="M6 3h9l4 4v14H6z" /><path d="M9 12h7M9 16h7" /></Svg>,
  Copy: () => <Svg><rect x="9" y="9" width="11" height="11" rx="2" /><path d="M5 15V5a1 1 0 011-1h10" /></Svg>,
  QR: () => <Svg><rect x="3" y="3" width="7" height="7" /><rect x="14" y="3" width="7" height="7" /><rect x="3" y="14" width="7" height="7" /><path d="M14 14h3v3M21 14v7h-7" /></Svg>,
  Edit: () => <Svg><path d="M4 20h4L19 9l-4-4L4 16z" /></Svg>,
  Trash: () => <Svg><path d="M4 7h16M10 11v6M14 11v6M6 7l1 13h10l1-13M9 7V4h6v3" /></Svg>,
  Close: () => <Svg><path d="M6 6l12 12M18 6L6 18" /></Svg>,
  Plus: () => <Svg><path d="M12 5v14M5 12h14" /></Svg>,
  Refresh: () => <Svg><path d="M20 12a8 8 0 11-2.3-5.6M20 4v5h-5" /></Svg>,
  Eye: () => <Svg><path d="M2 12s4-7 10-7 10 7 10 7-4 7-10 7S2 12 2 12z" /><circle cx="12" cy="12" r="3" /></Svg>,
  Globe: () => <Svg><circle cx="12" cy="12" r="9" /><path d="M3 12h18M12 3a14 14 0 010 18M12 3a14 14 0 000 18" /></Svg>,
  Convert: () => <Svg><path d="M4 7h13l-3-3M20 17H7l3 3" /></Svg>,
  Bot: () => <Svg><rect x="4" y="8" width="16" height="11" rx="3" /><path d="M12 4v4M9 13h.01M15 13h.01" /></Svg>,
}
