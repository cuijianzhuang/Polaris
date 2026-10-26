import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import '@fontsource-variable/inter'
import '@fontsource/ibm-plex-mono/400.css'
import '@fontsource/ibm-plex-mono/500.css'
import './styles.css'
import { loadLang } from './i18n'

// App and theme compute translated labels when their modules load, so they
// are imported only after the language dictionary is ready.
loadLang().then(async () => {
  const [{ default: App }, { applyTheme, loadTheme }] = await Promise.all([import('./App'), import('./theme')])
  applyTheme(loadTheme())
  createRoot(document.getElementById('root')!).render(<StrictMode><App /></StrictMode>)
})
