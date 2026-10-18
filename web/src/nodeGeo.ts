// Where a node sits on the world map. Pure functions, no DOM.
//
// Position, first match wins:
//   1. explicit lat / lng on the node
//   2. a city found in Region or Name (Chinese or English, see data/places.json)
//   3. the centre of the country given by the ISO code (or a country name)
// Otherwise the node is "unplaced".
import { MAP_SCALE, MAP_TX, MAP_TY } from './assets/world-map'
import countryData from './data/countries.json'
import places from './data/places.json'

export interface GeoInput { lat?: number | null; lng?: number | null; region?: string; name?: string; flag?: string }
export interface GeoPoint { lat: number; lng: number; source: 'coords' | 'city' | 'country'; cc: string }

// Natural Earth 1 (Šavrič et al. polynomial); lambda / phi in radians.
// Must stay identical to the formula in scripts/gen-world-map.mjs.
export function naturalEarth1Raw(lambda: number, phi: number): [number, number] {
  const p2 = phi * phi
  const p4 = p2 * p2
  return [
    lambda * (0.8707 - 0.131979 * p2 + p4 * (-0.013791 + p4 * (0.003971 * p2 - 0.001529 * p4))),
    phi * (1.007226 + p2 * (0.015085 + p4 * (-0.044475 + 0.028874 * p2 - 0.005916 * p4))),
  ]
}

// project maps degrees to the map's viewBox coordinates.
export function project(lat: number, lng: number): [number, number] {
  const [x, y] = naturalEarth1Raw((lng * Math.PI) / 180, (lat * Math.PI) / 180)
  return [MAP_SCALE * x + MAP_TX, MAP_TY - MAP_SCALE * y]
}

const countries = countryData as Record<string, number[]>

// ISO numeric id of a country that has an outline on the map (0 = none).
export function countryNumeric(cc: string): number {
  return countries[cc]?.[2] ?? 0
}


const latinRe = (a: string) => new RegExp('(^|[^a-z])' + a.replace(/[.*+?^${}()|[\]\\]/g, '\\$&').replace(/ /g, '[\\s_-]*') + '([^a-z]|$)')
const hasHan = (s: string) => /[\u3400-\u9fff]/.test(s)

// Longest alias wins, so "san jose" is not shadowed by a shorter code.
const cities: { alias: string; test: (s: string) => boolean; c: [number, number, string] }[] = []
for (const [aliases, lat, lng, cc] of places.cities as [string, number, number, string][]) {
  for (const a of aliases.split(',')) {
    const alias = a.trim().toLowerCase()
    if (!alias) continue
    if (hasHan(alias)) cities.push({ alias, test: (s) => s.includes(alias), c: [lat, lng, cc] })
    else { const re = latinRe(alias); cities.push({ alias, test: (s) => re.test(s), c: [lat, lng, cc] }) }
  }
}
cities.sort((a, b) => b.alias.length - a.alias.length)

const countryNames = Object.entries(places.countries as Record<string, string>).map(([alias, cc]) => ({
  alias, cc, test: hasHan(alias) ? (s: string) => s.includes(alias) : ((re) => (s: string) => re.test(s))(latinRe(alias)),
})).sort((a, b) => b.alias.length - a.alias.length)

function findCity(text: string, flag: string): GeoPoint | null {
  const s = text.toLowerCase()
  if (!s) return null
  for (const k of cities) {
    // A node flagged for another country is not placed by a city of this one.
    if (flag && k.c[2] !== flag) continue
    if (k.test(s)) return { lat: k.c[0], lng: k.c[1], source: 'city', cc: k.c[2] }
  }
  return null
}

const validPos = (lat?: number | null, lng?: number | null) =>
  typeof lat === 'number' && typeof lng === 'number' && Number.isFinite(lat) && Number.isFinite(lng) && Math.abs(lat) <= 90 && Math.abs(lng) <= 180

// countryCode returns the ISO code of the node's country, if it can be told.
export function countryCode(n: GeoInput): string {
  const f = (n.flag ?? '').trim().toUpperCase()
  if (countries[f]) return f
  const city = findCity(n.region ?? '', '') ?? findCity(n.name ?? '', '')
  if (city) return city.cc
  const text = `${n.region ?? ''} ${n.name ?? ''}`.toLowerCase()
  return countryNames.find((k) => k.test(text))?.cc ?? ''
}

export function resolveNodeGeo(n: GeoInput): GeoPoint | null {
  const flag = (n.flag ?? '').trim().toUpperCase()
  const known = countries[flag] ? flag : ''
  if (validPos(n.lat, n.lng)) return { lat: n.lat as number, lng: n.lng as number, source: 'coords', cc: countryCode(n) }
  const city = findCity(n.region ?? '', known) ?? findCity(n.name ?? '', known)
  if (city) return city
  const cc = known || countryCode(n)
  const c = countries[cc]
  return c ? { lat: c[0], lng: c[1], source: 'country', cc } : null
}

export interface Pt { id: string; x: number; y: number }

// clusterPoints groups points closer than minDist (same unit as x / y).
export function clusterPoints(pts: Pt[], minDist: number): Pt[][] {
  const groups: { cx: number; cy: number; items: Pt[] }[] = []
  for (const p of pts) {
    const g = groups.find((q) => Math.hypot(q.cx - p.x, q.cy - p.y) < minDist)
    if (g) {
      g.items.push(p)
      g.cx = g.items.reduce((s, i) => s + i.x, 0) / g.items.length
      g.cy = g.items.reduce((s, i) => s + i.y, 0) / g.items.length
    } else groups.push({ cx: p.x, cy: p.y, items: [p] })
  }
  return groups.map((g) => g.items)
}

// ringOffsets places n markers around a centre: one ring of up to 7, then
// wider rings. radius is the marker spacing unit (px).
export function ringOffsets(n: number, radius: number): [number, number][] {
  if (n <= 1) return [[0, 0]]
  const out: [number, number][] = []
  let ring = 0
  let left = n
  while (left > 0) {
    const cap = ring === 0 ? Math.min(7, n) : 7 + ring * 6
    const m = Math.min(cap, left)
    const r = radius * (1 + ring * 0.95) * (m === 2 ? 0.7 : 1)
    for (let i = 0; i < m; i++) {
      const a = -Math.PI / 2 + (2 * Math.PI * i) / m + ring * 0.4
      out.push([r * Math.cos(a), r * Math.sin(a)])
    }
    left -= m
    ring++
  }
  return out
}
