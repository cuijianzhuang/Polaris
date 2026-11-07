"""CDN tab isolation, account actions, avatar failure and folded/mobile reachability."""
import hashlib, json, os, shutil
from pathlib import Path
from urllib.parse import urlparse
from playwright.sync_api import sync_playwright, expect

proxy_source = Path(__file__).with_name('proxy-chain-ui-smoke.py')
proxy = {'__file__': str(proxy_source)}
exec(proxy_source.read_text().split('\nwith sync_playwright() as p:')[0], proxy)
cdn_source = Path(__file__).with_name('cdn-domains-ui-smoke.py')
cdn = {'__file__': str(cdn_source)}
exec(cdn_source.read_text().split('\nwith sync_playwright() as p:')[0], cdn)
BASE = os.environ.get('POLARIS_WEB_TEST_URL', 'http://127.0.0.1:5187').rstrip('/')
OUT = Path(os.environ.get('POLARIS_UI_ARTIFACTS', '/tmp/polaris-account-navigation'))
OUT.mkdir(parents=True, exist_ok=True)

class Fixture(proxy['ProxyFixture']):
    def __init__(self):
        super().__init__()
        self.cdn = cdn['Fixture']()
        self.cdn.profile['name'] = 'CDN dedicated fixture'
        self.email = ''
        self.role = 'owner'
        self.hold_after_save = False
        self.pending_profiles = []
        self.name = 'administrator-with-a-long-display-name'
    def route(self, route):
        request = route.request
        path = urlparse(request.url).path.removeprefix('/api/v1')
        if path == '/auth/avatar' and request.method == 'PUT':
            self.email = request.post_data_json['email'].strip().lower()
            self.hold_profile = self.hold_after_save
            return route.fulfill(json={'avatarEmail':self.email,'avatarURL':self.avatar()})
        if path == '/auth/me':
            profile = {'username':self.name,'role':self.role,'avatarEmail':self.email,'avatarURL':self.avatar()}
            if getattr(self,'hold_profile',False):
                self.pending_profiles.append((route,profile)); return
            return route.fulfill(json=profile)
        if path in ['/staff/nodes','/auth/sessions']:return route.fulfill(json=[])
        if path == '/auth/passkeys':return route.fulfill(json={'available':False,'items':[]})
        if path.startswith('/cdn'):
            return self.cdn.route(route)
        return super().route(route)
    def avatar(self):
        return 'https://www.gravatar.com/avatar/'+hashlib.sha256(self.email.encode()).hexdigest()+'?s=128&d=identicon&r=g' if self.email else ''

def reachable(locator):
    locator.scroll_into_view_if_needed()
    assert locator.evaluate('''el => {
      const r = el.getBoundingClientRect(), hit = document.elementFromPoint(r.x+r.width/2,r.y+r.height/2);
      return r.left >= 0 && r.right <= innerWidth+1 && r.top >= 0 && r.bottom <= innerHeight+1 && el.contains(hit)
    }'''), 'account action is clipped or covered'

with sync_playwright() as p:
    for engine in os.environ.get('POLARIS_TEST_ENGINES','chromium,webkit').split(','):
        launch = dict(headless=True)
        if engine == 'chromium':
            launch.update(executable_path=os.environ.get('POLARIS_CHROMIUM_BIN') or shutil.which('chromium'),args=['--no-sandbox','--disable-dev-shm-usage'])
        browser = getattr(p,engine).launch(**launch)
        for width,height,lang,mode,folded in [(1440,1000,'zh','light',False),(1280,900,'en','dark',False),(1440,800,'zh','dark',True),(390,844,'zh','light',False),(760,390,'en','dark',False)]:
            fixture = Fixture()
            page = browser.new_page(viewport=dict(width=width,height=height),color_scheme=mode)
            page.set_default_timeout(10000)
            page.add_init_script("localStorage.setItem('polaris.token','fixture');localStorage.setItem('polaris.lang',%s);localStorage.setItem('polaris.refresh','0');localStorage.setItem('polaris.sidebar',%s);localStorage.setItem('polaris.theme',%s)" % (json.dumps(lang),json.dumps('folded' if folded else 'expanded'),json.dumps(json.dumps(dict(style='default',mode=mode,accent='ice')))))
            errors = []; page.on('pageerror',lambda error:errors.append(str(error)))
            page.route('**/api/v1/**',fixture.route)
            failed_images = [False]
            def avatar(route):
                assert not route.request.headers.get('referer'), 'Gravatar received the page referrer'
                assert '@' not in route.request.url, 'Gravatar received a plaintext email'
                if failed_images[0]: return route.abort()
                route.fulfill(content_type='image/svg+xml',body='<svg xmlns="http://www.w3.org/2000/svg" width="128" height="128"><rect width="128" height="128" fill="#268d87"/></svg>')
            page.route('https://www.gravatar.com/**',avatar)
            label = lambda zh,en: zh if lang=='zh' else en
            page.goto(BASE+'/chains')
            expect(page.locator('.chain-list')).to_be_visible()
            expect(page.locator('.chain-list')).to_contain_text('hk-to-tw')
            expect(page.locator('.cdn-detail')).to_have_count(0)
            expect(page.locator('main').get_by_text('CDN dedicated fixture',exact=False)).to_have_count(0)
            expect(page.get_by_role('button',name=label('+ CDN 配置档','+ CDN profile'),exact=True)).to_have_count(0)
            page.get_by_role('tab',name=label('CDN 配置档','CDN profile'),exact=True).click()
            expect(page.locator('.cdn-detail')).to_contain_text('CDN dedicated fixture')
            expect(page.get_by_role('button',name=label('+ 新建链路','+ New chain'),exact=True)).to_have_count(0)
            page.get_by_role('tab',name=label('链路','Chains'),exact=True).click()
            expect(page.locator('.cdn-detail')).to_have_count(0)
            if width <= 760:
                page.get_by_role('button',name=label('打开菜单','Open menu'),exact=True).click()
            trigger = page.locator('.side-account-trigger')
            reachable(trigger); trigger.click()
            panel = page.locator('.side-account-panel')
            expect(panel).to_be_visible()
            reachable(panel.get_by_role('button',name=label('账号头像','Account avatar'),exact=True))
            page.screenshot(path=str(OUT/f'account-{engine}-{width}-{mode}-{folded}.png'),animations='disabled')
            page.keyboard.press('Escape')
            expect(trigger).to_have_attribute('aria-expanded','false')
            expect(trigger).to_be_focused()
            trigger.click()
            page.mouse.click(6, height - 6)
            expect(trigger).to_have_attribute('aria-expanded','false')
            for email,fail in [('Person+avatar@Example.com',False),('fallback@example.com',True)]:
                failed_images[0] = fail
                trigger.click()
                panel.get_by_role('button',name=label('账号头像','Account avatar'),exact=True).click()
                dialog = page.get_by_role('dialog')
                dialog.get_by_label(label('头像邮箱','Avatar email'),exact=True).fill(email)
                dialog.get_by_role('button',name=label('保存头像设置','Save avatar settings'),exact=True).click()
                expect(dialog).not_to_be_visible()
                if fail:
                    expect(trigger.locator('img')).to_have_count(0)
                else:
                    expect(trigger.locator('img')).to_have_attribute('src',fixture.avatar())
                    trigger.locator('img').evaluate('(el) => el.decode().catch(()=>{})')
            trigger.click()
            theme = panel.locator('.theme-btn:not(.lang-btn)')
            previous = theme.get_attribute('aria-label'); theme.click()
            expect(theme).not_to_have_attribute('aria-label',previous)
            page.keyboard.press('Escape')
            reachable(page.locator('.side-quick-logout'))
            page.locator('.side-quick-logout').click()
            expect(page.get_by_role('button',name=label('登录','Sign in'),exact=True)).to_be_visible()
            assert page.evaluate('localStorage.getItem("polaris.token")') is None
            assert not errors, errors
            page.close()
            print(engine,width,height,lang,mode,folded,'CDN isolation, account actions, Gravatar and fallback OK',flush=True)
        # A delayed profile refresh after saving must not erase the next draft.
        fixture = Fixture(); fixture.role = 'viewer'; fixture.hold_after_save = True
        page = browser.new_page(viewport=dict(width=390,height=844))
        page.add_init_script("localStorage.setItem('polaris.token','fixture');localStorage.setItem('polaris.lang','zh')")
        page.route('**/api/v1/**',fixture.route)
        page.route('https://www.gravatar.com/**',lambda route:route.abort())
        page.goto(BASE+'/account')
        page.get_by_role('button',name='我的账号',exact=True).click()
        field=page.get_by_label('头像邮箱',exact=True)
        save=page.get_by_role('button',name='保存头像设置',exact=True)
        field.fill('saved@example.com')
        with page.expect_request(lambda request:urlparse(request.url).path.endswith('/auth/me')):
            save.click()
        expect(page.locator('.avatar-settings')).to_have_attribute('aria-busy','false')
        field.fill('next-draft@example.com')
        expect(save).to_be_enabled()
        with page.expect_response(lambda response:urlparse(response.url).path.endswith('/auth/me')) as refreshed:
            fixture.hold_profile = False
            for route,profile in fixture.pending_profiles:route.fulfill(json=profile)
        refreshed.value.json()
        expect(page.locator('.avatar-settings')).not_to_have_attribute('aria-busy','true')
        # Wait for the preview response to be applied before inspecting the draft.
        expect(page.locator('.avatar-settings-preview img')).to_have_count(0)
        page.evaluate('() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve)))')
        expect(field).to_have_value('next-draft@example.com')
        expect(save).to_be_enabled()
        page.close()
        print(engine,'late profile refresh preserves the next avatar draft',flush=True)
        browser.close()
