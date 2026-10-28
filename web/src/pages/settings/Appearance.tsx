import { ProTag, useFeature } from '../../components/License'
import { del, put } from '../../api'
import { Icon } from '../../components/Icons'
import { accents, styles, useTheme, type ThemeMode } from '../../theme'
import { logoURL, refreshBrand, useBrand } from '../../brand'
import { Field, useAction, useConfirm } from '../../components/ui'
import { t } from '../../i18n'
import ThemePreview from '../../components/ThemePreview'

// Appearance: mode, accent and surface style are per browser; the logo is
// the panel's (paid features "themes" and "branding").
export function Appearance() {
  const [th, setT] = useTheme()
  const themesOK = useFeature('themes')
  const modes: { id: ThemeMode; label: string }[] = [{ id: 'light', label: t('浅色') }, { id: 'dark', label: t('深色') }, { id: 'system', label: t('跟随系统') }]
  const special = th.style === 'blackgold' && themesOK
  const activeStyle = th.style === 'blackgold' && !themesOK ? 'default' : th.style
  const shownMode = special ? 'dark' : th.mode
  return (
    <>
      <section className="card appearance">
        <div className="appearance-heading"><div><h2>{t('外观')}</h2><p>{t('让界面适合你的工作习惯。')}</p></div><span className="appearance-local">{t('仅保存在当前浏览器')}</span></div>
        <Field label={t('主题风格')} hint={t('三套基础主题均支持明暗模式；配色可跟随风格或自选。黑金需要专业版。')}>
          <div className="style-cards">
            {styles.map((x) => {
              const locked = x.premium && !themesOK
              const on = activeStyle === x.id
              return (
                <button key={x.id} type="button" className={'style-card style-' + x.id + (on ? ' on' : '')} aria-pressed={on} disabled={locked}
                  title={locked ? t('需要专业版许可证（许可证页面）') : x.label} onClick={() => setT({ ...th, style: x.id })}>
                  <span className="style-preview" aria-hidden="true"><i /><i /><i /></span>
                  <span className="style-label"><b>{x.label}{locked ? ' 🔒' : ''}</b><small>{x.description}</small></span>
                </button>
              )
            })}
          </div>
        </Field>
        <div className="appearance-layout">
          <div className="appearance-controls">
            <Field label={t('主题模式')} hint={special ? t('当前专属主题固定使用深色及专属配色，切换到基础主题后可调整。') : t('跟随系统会自动匹配设备的明暗模式。')}>
              <div className="seg c3 fixed">
                {modes.map((m) => <button key={m.id} type="button" disabled={special} className={'opt' + (shownMode === m.id ? ' on' : '')} aria-pressed={shownMode === m.id} onClick={() => setT({ ...th, mode: m.id })}>{m.label}</button>)}
              </div>
            </Field>
            <Field label={t('主题色')}>
              <div className="swatches">
                {accents.map((a) => <button key={a.id} type="button" disabled={special} className={'accent-swatch' + (!special && th.accent === a.id ? ' on' : '')}
                  title={a.label} aria-label={t('主题色：{name}', { name: a.label })} aria-pressed={!special && th.accent === a.id} onClick={() => setT({ ...th, accent: a.id })}>
                  <i style={{ background: a.color }} aria-hidden="true" /><span>{a.label}</span><span className="accent-selected" aria-hidden="true">{!special && th.accent === a.id ? '✓' : ''}</span>
                </button>)}
              </div>
            </Field>
          </div>
          <ThemePreview />
        </div>
      </section>
      <BrandLogoCard />
    </>
  )
}

// BrandLogoCard uploads the panel's own logo (sidebar, login page, favicon).
export function BrandLogoCard() {
  const ok = useFeature('branding')
  const { logo } = useBrand()
  const { busy, run } = useAction()
  const confirm = useConfirm()
  const pick = (file?: File) => {
    if (!file) return
    if (file.size > 256 << 10) { run(async () => { throw new Error(t('图片不能超过 256 KB')) }); return }
    const r = new FileReader()
    r.onload = () => run(async () => { await put('/brand/logo', { data: String(r.result) }); refreshBrand() }, t('Logo 已更新'))
    r.readAsDataURL(file)
  }
  return (
    <section className="card" style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
      <h2>{t('自定义 Logo')}<ProTag on={ok} /></h2>
      <div className="row" style={{ gap: 16, flexWrap: 'wrap' }}>
        <div className="logo-preview">{logo ? <img src={logoURL(logo)} alt="" /> : <Icon.LogoTile size={56} />}</div>
        <div style={{ display: 'flex', flexDirection: 'column', gap: 8, flex: 1, minWidth: 200 }}>
          <div className="small mute">{t('显示在侧边栏、登录页和浏览器标签页图标。支持 PNG、JPG、WebP、GIF、SVG，不超过 256 KB，建议正方形、至少 128×128。')}</div>
          <div className="row" style={{ gap: 8 }}>
            <label className={'btn' + (ok && !busy ? '' : ' disabled')} aria-disabled={!ok || busy}>
              {logo ? t('更换') : t('上传')}
              <input type="file" accept="image/png,image/jpeg,image/webp,image/gif,image/svg+xml" hidden disabled={!ok || busy}
                onChange={(e) => { pick(e.target.files?.[0]); e.target.value = '' }} />
            </label>
            {logo && <button type="button" className="btn" disabled={busy} onClick={async () => {
              if (!await confirm(t('恢复为默认的 Polaris Logo？'), { ok: t('恢复默认') })) return
              run(async () => { await del('/brand/logo'); refreshBrand() }, t('已恢复默认 Logo'))
            }}>{t('恢复默认')}</button>}
          </div>
        </div>
      </div>
    </section>
  )
}
