# Visual and interaction regression for the console shell and appearance page.
# Run against Vite with Playwright + Chromium; all API data below is synthetic.
import json, os, re, shutil
from pathlib import Path
from urllib.parse import urlparse
from playwright.sync_api import sync_playwright, expect

fixtures = {}
exec(Path(__file__).with_name('operations-ui-smoke.py').read_text().split('with sync_playwright() as p:')[0], fixtures)
BaseFixture = fixtures['Fixture']
BASE = os.environ.get('POLARIS_WEB_TEST_URL', 'http://127.0.0.1:5187').rstrip('/')
OUT = Path(os.environ.get('POLARIS_APPEARANCE_ARTIFACTS', '/tmp/polaris-appearance-ui'))
OUT.mkdir(exist_ok=True, parents=True)

class Fixture(BaseFixture):
    def __init__(self, premium=False):
        super().__init__()
        self.premium = premium
    def route(self, route):
        path = urlparse(route.request.url).path
        if path == '/api/v1/probe':
            from datetime import datetime, timezone
            now=datetime.now(timezone.utc).isoformat()
            node=dict(id='preview',name='Theme fixture',region='HK',flag='HK',online=True,status='online',cpu=24,mem=42,rxBps=10000,txBps=20000,uptime=3600,load1=.5,netUsed=0,netLimit=0,lastSeen=now,telemetryFresh=True,disk=20,diskTotal=100,ipv4=True,ipv6=False,latency={},probeTargets=[])
            return route.fulfill(content_type='application/json',body=json.dumps(dict(nodes=[node],targets=[],at=now,probeEnabled=True,statusPage=False,showTraffic=True)))
        if path == '/api/v1/license':
            return route.fulfill(content_type='application/json', body=json.dumps(dict(
                edition='pro' if self.premium else 'community',
                state=dict(plan='pro' if self.premium else 'community', features=['themes'] if self.premium else [], warnings=[], limits={}), usage={})))
        return super().route(route)

def no_overflow(page):
    assert page.evaluate('document.documentElement.scrollWidth <= innerWidth + 1'), 'page overflowed'

def contrast(colors):
    def luminance(value):
        rgb=[float(v)/255 for v in re.findall(r'[\d.]+', value)[:3]]
        rgb=[v/12.92 if v<=.04045 else ((v+.055)/1.055)**2.4 for v in rgb]
        return sum(v*w for v,w in zip(rgb,[.2126,.7152,.0722]))
    values=[luminance(colors[k]) for k in ['foreground','background']]
    return (max(values)+.05)/(min(values)+.05)

with sync_playwright() as p:
    browser = p.chromium.launch(executable_path=os.environ.get('POLARIS_CHROMIUM_BIN') or shutil.which('chromium'), headless=True,
                                args=['--no-sandbox', '--disable-dev-shm-usage'])
    for width in [390, 1440]:
        for language in ['zh', 'en']:
            f = Fixture()
            page = browser.new_page(viewport=dict(width=width, height=1000), locale='zh-CN' if language=='zh' else 'en-US', is_mobile=width<760, has_touch=width<760)
            page.set_default_timeout(10000)
            page.add_init_script("localStorage.setItem('polaris.token','fixture');localStorage.setItem('polaris.refresh','0');localStorage.setItem('polaris.lang',%s)" % json.dumps(language))
            errors = []
            page.on('pageerror', lambda e: errors.append(str(e)))
            page.route('**/api/v1/**', f.route)
            page.goto(BASE + '/settings#appearance')
            root = page.locator('html')
            expect(page.locator('.theme-demo')).to_be_visible()
            expect(root).to_have_attribute('data-accent','amber')
            expect(root).to_have_attribute('data-style','warm')
            expect(page.locator('.style-warm')).to_be_enabled()
            expect(page.locator('.style-warm')).to_have_attribute('aria-pressed','true')
            expect(page.locator('.style-card[aria-pressed="true"]')).to_have_count(1)
            action=page.locator('.theme-demo-actions > .primary').evaluate('(e)=>({foreground:getComputedStyle(e).color,background:getComputedStyle(e).backgroundColor})')
            assert contrast(action)>=4.5, action
            expect(page.locator('.style-blackgold')).to_be_disabled()
            # All free styles resolve their recommended accent without a license.
            for style,accent in [('default','slate'),('midnight','ice'),('warm','amber')]:
                page.locator('.style-'+style).click()
                expect(root).to_have_attribute('data-accent',accent)
                assert page.evaluate("document.documentElement.dataset.style || ''") == ('' if style=='default' else style)
                expect(page.locator('.style-card[aria-pressed="true"]')).to_have_count(1)
                for mode in ['light','dark']:
                    label = ('浅色' if mode=='light' else '深色') if language=='zh' else mode.title()
                    page.locator('.appearance-controls').get_by_role('button',name=label,exact=True).click()
                    expect(root).to_have_attribute('data-mode',mode)
                    action=page.locator('.theme-demo-actions > .primary').evaluate('(e)=>({foreground:getComputedStyle(e).color,background:getComputedStyle(e).backgroundColor})')
                    assert contrast(action)>=4.5, (style,mode,action)
                    no_overflow(page)
            # Actual mode, OS following and labeled palette choices apply at once.
            page.locator('.appearance-controls').get_by_role('button', name='深色' if language=='zh' else 'Dark', exact=True).click()
            expect(root).to_have_attribute('data-mode','dark')
            action=page.locator('.theme-demo-actions > .primary').evaluate('(e)=>({foreground:getComputedStyle(e).color,background:getComputedStyle(e).backgroundColor})')
            assert contrast(action)>=4.5, action
            page.locator('.appearance-controls').get_by_role('button', name='跟随系统' if language=='zh' else 'System', exact=True).click()
            page.emulate_media(color_scheme='light')
            expect(root).to_have_attribute('data-mode','light')
            page.emulate_media(color_scheme='dark')
            expect(root).to_have_attribute('data-mode','dark')
            page.get_by_role('button', name='主题色：海蓝' if language=='zh' else 'Accent color: Blue', exact=True).click()
            expect(root).to_have_attribute('data-accent','blue')
            saved = page.evaluate("JSON.parse(localStorage.getItem('polaris.theme'))")
            assert saved['mode']=='system' and saved['accent']=='blue' and saved['style']=='warm', saved
            page.reload()
            expect(root).to_have_attribute('data-accent','blue')
            expect(root).to_have_attribute('data-mode','dark')
            page.locator('.style-midnight').click()
            expect(root).to_have_attribute('data-accent','blue')
            page.get_by_role('button',name='主题色：跟随风格' if language=='zh' else 'Accent color: Match style',exact=True).click()
            expect(root).to_have_attribute('data-accent','ice')
            page.reload()
            expect(root).to_have_attribute('data-accent','ice')
            expect(root).to_have_attribute('data-style','midnight')
            saved=page.evaluate("JSON.parse(localStorage.getItem('polaris.theme'))")
            assert saved['accent']=='auto' and saved['style']=='midnight', saved
            # The preview uses real token values, not a fixed light mockup.
            colors = page.locator('.theme-demo').evaluate('(e)=>({preview:getComputedStyle(e.querySelector(".theme-demo-main")).backgroundColor,real:getComputedStyle(document.body).backgroundColor})')
            assert colors['preview']==colors['real'], colors
            if width < 900:
                chooser=page.get_by_role('combobox',name='设置分区' if language=='zh' else 'Settings sections',exact=True)
                expect(chooser).to_be_visible()
                chooser.select_option('notifications')
                expect(page).to_have_url(BASE+'/settings#notifications')
                chooser.select_option('appearance')
                expect(page.locator('.theme-demo')).to_be_visible()
                search=page.locator('.mtop').get_by_role('button',name='搜索' if language=='zh' else 'Search',exact=True)
            else:
                expect(page.locator('.snav')).to_be_visible()
                page.get_by_role('button',name='收起侧栏' if language=='zh' else 'Collapse sidebar',exact=True).click()
                expect(page.locator('.app')).to_have_class('app folded')
                no_overflow(page)
                page.get_by_role('button',name='展开侧栏' if language=='zh' else 'Expand sidebar',exact=True).click()
                search=page.locator('.workspace-bar').get_by_role('button',name='搜索' if language=='zh' else 'Search',exact=True)
            search.click()
            expect(page.get_by_role('dialog',name='快速跳转' if language=='zh' else 'Quick jump',exact=True)).to_be_visible()
            search_input=page.get_by_role('dialog').get_by_role('textbox')
            expect(search_input).to_be_focused()
            page.keyboard.press('Shift+Tab')
            expect(page.get_by_role('dialog').get_by_role('button').last).to_be_focused()
            page.keyboard.press('Tab')
            expect(search_input).to_be_focused()
            page.keyboard.press('Escape')
            expect(page.get_by_role('dialog')).to_be_hidden()
            expect(search).to_be_focused()
            page.keyboard.press('Control+k')
            expect(page.get_by_role('dialog')).to_be_visible()
            page.keyboard.press('Escape')
            expect(page.get_by_role('dialog')).to_be_hidden()
            no_overflow(page)
            page.keyboard.press('Control+k')
            command=page.get_by_role('dialog').get_by_role('button',name='节点列表' if language=='zh' else 'Node list',exact=True)
            command.focus()
            page.keyboard.press('Enter')
            expect(page).to_have_url(BASE+'/nodes')
            page.goto(BASE+'/settings#appearance')
            expect(page.locator('.theme-demo')).to_be_visible()
            assert not f.writes, f.writes
            assert not errors, errors
            page.screenshot(path=str(OUT/f'appearance-{width}-{language}.png'),full_page=True,animations='disabled')
            page.close()
            print(width,language,'shell search, responsive navigation, theme persistence, OS mode and premium lock passed',flush=True)
    # Exclusive styles keep their original entitlement and restore base preferences.
    f=Fixture(premium=True)
    page=browser.new_page(viewport=dict(width=1440,height=1000),locale='zh-CN')
    page.add_init_script("localStorage.setItem('polaris.token','fixture');localStorage.setItem('polaris.lang','zh')")
    page.route('**/api/v1/**',f.route)
    page.goto(BASE+'/settings#appearance')
    gold=page.locator('.style-blackgold')
    expect(gold).to_be_enabled()
    page.get_by_role('button',name='浅色',exact=True).click()
    gold.click()
    expect(page.locator('html')).to_have_attribute('data-style','blackgold')
    expect(page.locator('html')).to_have_attribute('data-mode','dark')
    dark=page.get_by_role('button',name='深色',exact=True)
    expect(dark).to_be_disabled()
    expect(dark).to_have_attribute('aria-pressed','true')
    expect(page.locator('.accent-swatch[aria-pressed="true"]')).to_have_count(0)
    page.screenshot(path=str(OUT/'appearance-blackgold.png'),full_page=True,animations='disabled')
    page.locator('.style-warm').click()
    expect(page.locator('html')).to_have_attribute('data-style','warm')
    expect(page.locator('html')).to_have_attribute('data-mode','light')
    expect(page.get_by_role('button',name='浅色',exact=True)).to_be_enabled()
    gold.click()
    page.goto(BASE+'/probe')
    expect(page.locator('.pv-card')).to_be_visible()
    tokens=page.locator('.pv').evaluate('(e)=>{const local=getComputedStyle(e),root=getComputedStyle(document.documentElement);return {surface:local.getPropertyValue("--pv-card").trim(),expected:root.getPropertyValue("--card").trim(),accent:local.getPropertyValue("--pv-cyan").trim(),expectedAccent:root.getPropertyValue("--pri-ink").trim()}}')
    assert tokens['surface']==tokens['expected'] and tokens['accent']==tokens['expectedAccent'], tokens
    f.premium=False
    page.goto(BASE+'/settings#appearance')
    expect(page.locator('.style-blackgold')).to_be_disabled()
    expect(page.locator('.style-default')).to_have_attribute('aria-pressed','true')
    assert page.evaluate("document.documentElement.dataset.style || ''") == ''
    page.close()
    # Existing saved choices are not migrated to the new default. A free warm
    # style remains effective after community license data has loaded.
    for style,accent in [('default','teal'),('warm','amber'),('midnight','ice')]:
        f=Fixture()
        page=browser.new_page()
        value=json.dumps(dict(mode='dark',style=style,accent=accent))
        page.add_init_script("localStorage.setItem('polaris.token','fixture');localStorage.setItem('polaris.theme',%s)" % json.dumps(value))
        page.route('**/api/v1/**',f.route)
        page.goto(BASE+'/settings#appearance')
        expect(page.locator('.style-blackgold')).to_be_disabled()
        expect(page.locator('html')).to_have_attribute('data-mode','dark')
        expect(page.locator('html')).to_have_attribute('data-accent',accent)
        assert page.evaluate("document.documentElement.dataset.style || ''") == ('' if style=='default' else style)
        assert page.evaluate("localStorage.getItem('polaris.theme')") == value
        page.close()
    browser.close()
    print('All free palettes, automatic/custom accents, saved choices and exclusive theme entitlement passed')
