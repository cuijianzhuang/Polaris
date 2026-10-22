import g1 from './g1'
import g2 from './g2'
import g3 from './g3'
import g4 from './g4'
import g5 from './g5'
import g6 from './g6'
import g7 from './g7'
import g8 from './g8'
import g9 from './g9'
import core from './core'

// English UI strings keyed by the Chinese source text, split by area so
// files can be edited independently.
const en: Record<string, string> = { ...core, ...g1, ...g2, ...g3, ...g4, ...g5, ...g6, ...g7, ...g8, ...g9 }
export default en
