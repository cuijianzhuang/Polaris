# Short-screen navigation and native menu interaction regressions. Synthetic API
# data only: opening/cancelling bulk creation must never submit a user change.
import json, os, shutil
from pathlib import Path
from urllib.parse import urlparse
from playwright.sync_api import sync_playwright, expect

scope = {'__file__': str(Path(__file__).with_name('appearance-ui-smoke.py').resolve())}
exec(Path(scope['__file__']).read_text().split('\nwith sync_playwright() as p:')[0], scope)
BaseFixture = scope['Fixture']
BASE = os.environ.get('POLARIS_WEB_TEST_URL', 'http://127.0.0.1:5187').rstrip('/')
OUT = Path(os.environ.get('POLARIS_DETAILS_ARTIFACTS', '/tmp/polaris-ui-details'))
OUT.mkdir(parents=True, exist_ok=True)

class Fixture(BaseFixture):
    def route(self, route):
        if urlparse(route.request.url).path == '/api/v1/users':
            return route.fulfill(json=dict(items=[], total=0, summary=dict(all=0)))
        return super().route(route)

def in_view(locator):
    assert locator.evaluate('(e)=>{const r=e.getBoundingClientRect();return r.top>=0 && r.bottom<=innerHeight && r.left>=0 && r.right<=innerWidth}'), 'control left the viewport'

def no_overflow(page):
    assert page.evaluate('document.documentElement.scrollWidth<=innerWidth+1'), 'document overflow'

with sync_playwright() as p:
    for engine in ['chromium', 'webkit']:
        options = dict(headless=True)
        if engine == 'chromium':
            options.update(executable_path=os.environ.get('POLARIS_CHROMIUM_BIN') or shutil.which('chromium'), args=['--no-sandbox','--disable-dev-shm-usage'])
        browser = getattr(p, engine).launch(**options)
        for width in [390, 768, 1440]:
            for language in ['zh', 'en']:
                for mode in ['light', 'dark']:
                    case = f'{engine}-{width}-{language}-{mode}'
                    f = Fixture()
                    page = browser.new_page(viewport=dict(width=width,height=560), is_mobile=width<760, has_touch=width<760)
                    page.set_default_timeout(10000)
                    errors=[]
                    page.on('pageerror', lambda e: errors.append(str(e)))
                    page.add_init_script("localStorage.setItem('polaris.token','fixture');localStorage.setItem('polaris.refresh','0');localStorage.setItem('polaris.lang',%s);localStorage.setItem('polaris.theme',%s)" % (json.dumps(language), json.dumps(json.dumps(dict(mode=mode, accent='auto', style='default')))))
                    page.route('**/api/v1/**', f.route)
                    page.goto(BASE+'/users')
                    heading=page.get_by_role('heading', name='用户' if language=='zh' else 'Users', exact=True)
                    expect(heading).to_be_visible()
                    side=page.locator('.side')
                    if width<760:
                        page.locator('.mtop').get_by_role('button',name='打开菜单' if language=='zh' else 'Open menu',exact=True).click()
                    for button in side.locator('.navgroup > button').all():
                        if button.get_attribute('aria-expanded')!='true': button.click()
                    menu=side.locator('.side-menu')
                    assert menu.evaluate('(e)=>e.scrollHeight>e.clientHeight'), 'fixture must exercise menu overflow'
                    menu.evaluate('(e)=>e.scrollTop=e.scrollHeight')
                    for item in [side.locator('.brand'), side.locator('.side-foot'), side.locator('.logout')]: in_view(item)
                    # The footer must be hittable, not merely visible behind the menu.
                    assert side.locator('.logout').evaluate('(e)=>{const r=e.getBoundingClientRect();return e.contains(document.elementFromPoint(r.x+r.width/2,r.y+r.height/2))}')
                    page.screenshot(path=str(OUT/(case+'-navigation.png')),animations='disabled')
                    if width<760:
                        side.get_by_role('button',name='关闭菜单' if language=='zh' else 'Close menu',exact=True).click()
                    else:
                        fold=side.locator('.side-fold')
                        fold.click()
                        expect(page.locator('.app')).to_have_class('app folded')
                        in_view(fold)
                        fold.click()
                        expect(page.locator('.app')).to_have_class('app')
                    more=page.locator('.top details.menu')
                    summary=more.locator('summary')
                    bulk=more.get_by_role('button',name='批量创建' if language=='zh' else 'Bulk create',exact=True)
                    expect(bulk).to_be_hidden()
                    summary.click()
                    expect(bulk).to_be_visible()
                    heading.click()
                    expect(bulk).to_be_hidden()
                    summary.click()
                    bulk.focus()
                    page.keyboard.press('Escape')
                    expect(bulk).to_be_hidden()
                    expect(summary).to_be_focused()
                    summary.click()
                    in_view(more.locator('.menu-list'))
                    bulk.click()
                    dialog=page.get_by_role('dialog',name='批量创建用户' if language=='zh' else 'Bulk create users',exact=True)
                    expect(dialog).to_be_visible()
                    page.keyboard.press('Escape')
                    expect(dialog).to_have_count(0)
                    no_overflow(page)
                    assert not errors, errors
                    assert not f.writes, f.writes
                    page.screenshot(path=str(OUT/(case+'-users.png')),animations='disabled')
                    page.close()
                    print(case+' passed',flush=True)
        browser.close()
print('24 short-screen/menu scenarios passed',flush=True)
