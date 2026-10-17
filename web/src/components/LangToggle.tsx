import { getLang, setLang } from '../i18n'

// LangToggle switches the UI between Chinese and English.
export default function LangToggle() {
  const en = getLang() === 'en'
  const hint = en ? '切换到中文' : 'Switch to English' // i18n-ignore: written in the target language
  return (
    <button type="button" className="theme-btn lang-btn" title={hint} aria-label={hint}
      onClick={() => setLang(en ? 'zh' : 'en')}>{en ? '中' /* i18n-ignore */ : 'EN'}</button>
  )
}
