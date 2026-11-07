import { useState, type CSSProperties } from 'react'

// Keep a local fallback while loading or when Gravatar is blocked. Page URLs
// and subscription credentials must never be sent as a referrer.
export default function Avatar({ src, name, size = 30, style }: { src?: string; name: string; size?: number; style?: CSSProperties }) {
  const [failed, setFailed] = useState('')
  return <span className="avatar profile-avatar" style={{ ...style, width: size, height: size }} title={name} aria-hidden="true">
    {(Array.from(name)[0] || '?').toUpperCase()}
    {src && src !== failed && <img key={src} alt="" referrerPolicy="no-referrer" loading="lazy" decoding="async" src={src} onError={() => setFailed(src)} />}
  </span>
}
