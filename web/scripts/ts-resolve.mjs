// node --import ./scripts/ts-resolve.mjs --experimental-strip-types --test …
// Lets pure-function tests import app modules written for Vite: extensionless
// relative imports ("./i18n" -> "./i18n.ts") and JSON imports without an
// import attribute.
import { registerHooks } from 'node:module'

registerHooks({
  resolve(specifier, context, next) {
    const relative = /^\.{1,2}\//.test(specifier)
    let r
    try {
      r = next(specifier, context)
    } catch (err) {
      if (!relative || /\.[cm]?[jt]sx?$/.test(specifier)) throw err
      for (const ext of ['.ts', '/index.ts']) {
        try { r = next(specifier + ext, context); break } catch { /* try the next form */ }
      }
      if (!r) throw err
    }
    if (r.url.endsWith('.json')) r = { ...r, importAttributes: { ...context.importAttributes, type: 'json' } }
    return r
  },
})
