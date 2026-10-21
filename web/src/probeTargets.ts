import catalog from './data/zstatic-probe-nodes.json'

export type ProbeLevel = 'province' | 'city'
export type ProbeCarrier = 'ct' | 'cu' | 'cm'
export type ProbeFamily = 'ipv4' | 'ipv6' | 'dual'
export type ProbePreset = {
  level: ProbeLevel; province: string; city: string; carrier: ProbeCarrier; family: ProbeFamily; addr: string
}

const carriers = { telecom: 'ct', unicom: 'cu', mobile: 'cm' } as const
const families = { ipv4: 'v4', ipv6: 'v6', dual: 'dualstack' } as const

// Snapshot of the public directory, including every listed endpoint. Province
// family variants follow the same substitution as the source website; city
// endpoints are IPv4 only and retain the exact port published in the directory.
export const probePresets: ProbePreset[] = [
  ...catalog.ProvinceNodes.flatMap((p) => Object.entries(carriers).flatMap(([key, carrier]) => {
    const addr = p.carriers[key as keyof typeof carriers]
    return addr ? Object.entries(families).map(([family, suffix]) => ({
      level: 'province' as const, province: p.province, city: '', carrier, family: family as ProbeFamily,
      addr: addr.replace('-v4.', `-${suffix}.`),
    })) : []
  })),
  ...catalog.CityNodes.map((p) => ({
    level: 'city' as const, province: p.province, city: p.city,
    carrier: carriers[p.carrier as keyof typeof carriers], family: 'ipv4' as const, addr: p.endpoint,
  })),
]

export const probeCatalogUpdatedAt = catalog.UpdatedAt.slice(0, 10)
