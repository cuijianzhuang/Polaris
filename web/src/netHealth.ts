import type { NetCollectionHealth } from './types'

export function receivedNetHealth(health?: NetCollectionHealth): NetCollectionHealth | undefined {
  return health ? { ...health, receivedAtMonotonic: performance.now() } : undefined
}
