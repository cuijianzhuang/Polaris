import { Icon } from './Icons'
import { t } from '../i18n'

// A non-interactive example using the same tokens as the actual panel.
export default function ThemePreview() {
  return <aside className="appearance-preview">
    <div className="appearance-preview-title"><h3>{t('界面预览')}</h3><span>{t('实时更新')}</span></div>
    <div className="theme-demo" role="img" aria-label={t('当前主题下的导航、卡片、按钮与状态预览')}>
      <div className="theme-demo-side"><Icon.Brand size={24} /><i /><i className="selected" /><i /><i /></div>
      <div className="theme-demo-main">
        <div className="theme-demo-heading"><b>Polaris</b><span className="theme-demo-dot" /></div>
        <div className="theme-demo-caption">{t('资源概览')}</div>
        <div className="theme-demo-stats"><div><small>CPU</small><strong>24.8<small>%</small></strong><i /></div><div><small>RAM</small><strong>42.1<small>%</small></strong><i /></div></div>
        <div className="theme-demo-node"><span className="theme-demo-node-icon"><Icon.Server /></span><span><b>{t('节点示例')}</b><small>Linux · 4C / 8G</small></span><span className="theme-demo-status">{t('在线')}</span></div>
        <div className="theme-demo-input">{t('搜索节点')}<Icon.Search /></div>
        <div className="theme-demo-actions"><span>{t('取消')}</span><span className="primary">{t('主要操作')}</span></div>
      </div>
    </div>
    <p className="appearance-preview-note">{t('示例数据，仅用于预览配色。主题选择会立即应用，无需保存。')}</p>
  </aside>
}
