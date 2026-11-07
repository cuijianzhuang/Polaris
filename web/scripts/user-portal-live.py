"""Real user portal navigation, registration gates, ownership and responsive UI."""
import json, os, shutil, hashlib
from pathlib import Path
from urllib.parse import urlsplit
from playwright.sync_api import sync_playwright, expect

base = os.environ['POLARIS_PORTAL_TEST_URL']
control = os.environ['POLARIS_PORTAL_TEST_CONTROL']
registration = os.environ['POLARIS_PORTAL_REGISTRATION'] == 'true'
codes = json.loads(os.environ['POLARIS_PORTAL_TEST_CODES'])
artifacts = Path(os.environ.get('POLARIS_CDN_ARTIFACTS', '/tmp/polaris-user-portal'))
artifacts.mkdir(parents=True, exist_ok=True)

def fits(page):
    assert page.evaluate('document.documentElement.scrollWidth <= innerWidth + 1'), 'page overflows horizontally'

with sync_playwright() as p:
    for engine in os.environ.get('POLARIS_TEST_ENGINES', 'chromium,webkit').split(','):
        options = dict(headless=True)
        if engine == 'chromium':
            options.update(executable_path=os.environ.get('POLARIS_CHROMIUM_BIN') or shutil.which('chromium'), args=['--no-sandbox', '--disable-dev-shm-usage'])
        browser = getattr(p, engine).launch(**options)
        # Existing sign-in when closed; explicit registration when open.
        context = browser.new_context(viewport=dict(width=390, height=844))
        page = context.new_page()
        page.route('https://www.gravatar.com/**', lambda route: route.fulfill(content_type='image/svg+xml', body='<svg xmlns="http://www.w3.org/2000/svg" width="80" height="80"><rect width="80" height="80" fill="#258f8a"/></svg>'))
        errors = []
        page.on('pageerror', lambda error: errors.append(str(error)))
        page.goto(base + '/u/register')
        if registration:
            expect(page.locator('#auth-title')).to_have_text('创建账号')
            expect(page.locator('[data-auth-mode="register"]')).to_have_attribute('aria-current', 'page')
        else:
            expect(page.locator('#registration-notice')).to_be_visible()
            expect(page.locator('[data-auth-mode="register"]')).to_have_count(0)
            result = context.request.post(base + '/u/email/code', data={'email':'bypass@example.com','purpose':'register'}, headers={'X-Polaris-Portal':'1'})
            assert result.status == 403 and result.json()['code'] == 'registration_closed'
            page.goto(base + '/u/login?next=/u/shop')
        expect(page.get_by_role('link', name='管理员')).to_have_count(0)
        page.get_by_role('button', name='发送验证码', exact=True).click()
        expect(page.locator('#merr')).to_have_text('请填写正确的邮箱地址')
        email = engine + '@example.com'
        page.locator('#memail').fill(email)
        page.locator('#mcode').fill(codes[email])
        page.screenshot(path=str(artifacts / f'user-auth-{engine}-{registration}.png'), full_page=True)
        fits(page)
        with page.expect_navigation():
            page.locator('#mgo').click()
        if registration:
            expect(page.locator('[data-view="overview"]')).to_be_visible()
        else:
            expect(page.get_by_role('heading', name='购买套餐', exact=True)).to_be_visible()
        if registration:
            expect(page.locator('.p-subscription')).to_contain_text('待开通')
            expect(page.locator('.p-subscription')).not_to_contain_text('不限流量')
        assert context.request.get(base + os.environ['POLARIS_PORTAL_ADMIN_API'] + '/users').status == 401
        context.close()

        for width in [360, 1440]:
            for theme in ['light', 'dark']:
                context = browser.new_context(viewport=dict(width=width, height=1000), color_scheme=theme)
                credentials = context.request.post(control+'/session').json()
                token = credentials['token']
                other_context = browser.new_context()
                other_context.add_cookies([dict(name='polaris_u',value=credentials['otherToken'],domain=urlsplit(base).hostname,path='/u',httpOnly=True,sameSite='Lax')])
                context.request.post(control+'/node',data={'state':'online'})
                context.add_cookies([dict(name='polaris_u',value=token,domain=urlsplit(base).hostname,path='/u',httpOnly=True,sameSite='Lax')])
                page = context.new_page()
                avatar_requests = []
                def avatar_image(route):
                    avatar_requests.append(route.request)
                    assert not route.request.headers.get('referer'), 'avatar leaked the page URL'
                    if theme == 'dark':
                        return route.abort()
                    route.fulfill(content_type='image/svg+xml', body='<svg xmlns="http://www.w3.org/2000/svg" width="80" height="80"><rect width="80" height="80" fill="#258f8a"/></svg>')
                page.route('https://www.gravatar.com/**', avatar_image)
                page.set_default_timeout(10000)
                page.on('pageerror', lambda error: errors.append(str(error)))
                page.goto(base + '/u')
                expect(page.locator('[data-view="overview"]')).to_be_visible()
                original = page.locator('.p-heading h1').first.inner_text()
                page.locator('.p-heading h1').first.evaluate('(el) => el.textContent = "欢迎回来，" + "longusername".repeat(3)')
                fits(page)
                page.locator('.p-heading h1').first.evaluate('(el, text) => el.textContent = text', original)
                expect(page.get_by_role('heading', name='欢迎回来，alice')).to_be_visible()
                avatar_hash = hashlib.sha256(b'alice.long.account.address.for.avatar@example.com').hexdigest()
                expect(page.locator('.p-user-menu img')).to_have_attribute('src', 'https://www.gravatar.com/avatar/'+avatar_hash+'?s=128&d=identicon&r=g')
                if theme == 'dark':
                    expect(page.locator('.p-user-menu img')).to_be_hidden()
                    expect(page.locator('.p-user-menu summary svg')).to_be_visible()
                page.get_by_role('button', name='账号菜单', exact=True).click()
                expect(page.locator('.p-user-menu')).to_have_attribute('open', '')
                page.keyboard.press('Escape')
                expect(page.locator('.p-user-menu')).not_to_have_attribute('open', '')
                page.get_by_role('button', name='账号菜单', exact=True).click()
                page.locator('.p-user-menu-panel a[href="/u#account"]').click()
                expect(page.locator('[data-view="account"]')).to_be_visible()
                page.get_by_role('button', name='账号菜单', exact=True).click()
                expect(page.locator('.p-user-menu-panel')).to_be_visible()
                page.locator('.p-user-menu-panel a[href="/u#orders"]').click()
                expect(page.locator('[data-view="orders"]')).to_be_visible()
                expect(page.locator('.p-user-menu')).not_to_have_attribute('open', '')
                page.evaluate('location.hash = "overview"')
                if width > 760:
                    email = page.locator('.p-account-label small')
                    assert email.evaluate('(el) => getComputedStyle(el).whiteSpace === "nowrap" && el.scrollWidth > el.clientWidth'), 'sidebar email is not truncated'

                expect(page.locator('.p-subscription')).to_contain_text('84 GB')
                expect(page.locator('a[href="/u/shop"]').first).to_have_attribute('href', '/u/shop')
                fits(page)
                if registration:
                    page.screenshot(path=str(artifacts / f'user-overview-{engine}-{width}-{theme}.png'), full_page=True)
                page.locator('.p-hero-actions a[href="#subscription"]').click()
                expect(page.locator('[data-view="subscription"]')).to_be_visible()
                expect(page.locator('#p-clients')).not_to_have_attribute('open', '')
                page.locator('#p-clients > summary').click()
                expect(page.locator('.p-client-grid a').first).to_be_visible()
                page.get_by_role('link', name='可用节点', exact=True).filter(visible=True).first.click()
                expect(page.locator('[data-view="nodes"]')).to_be_visible()
                expect(page.locator('.p-node-row')).to_have_count(2)
                assert 'Secret purchase' not in page.content() and 'bob-region-subscription-token' not in page.content()
                expect(page.locator('#p-node-counts')).to_have_text('业务已验证 0 · 连接通过 0 · 待检查 1 · 暂不可用 1')
                page.locator('#p-node-state').select_option('unavailable')
                expect(page.locator('.p-node-row:visible')).to_have_count(1)
                page.get_by_role('button', name='查看原因').click()
                expect(page.locator('#p-node-dialog')).to_be_visible()
                expect(page.locator('#p-dialog-reason')).to_contain_text('离线')
                fits(page)
                page.keyboard.press('Escape')
                expect(page.locator('#p-node-dialog')).not_to_be_visible()
                page.locator('#p-node-state').select_option('')
                page.locator('#p-node-region').select_option(label='香港')
                expect(page.locator('.p-node-row:visible')).to_have_count(1)
                page.locator('#p-node-region').select_option('')
                page.locator('#p-node-search').fill('does-not-exist')
                expect(page.locator('#p-node-empty')).to_be_visible()
                page.locator('#p-node-search').fill('')
                # Status refresh uses authoritative, user-scoped evidence without a full reload.
                context.request.post(control+'/node',data={'state':'offline'})
                page.get_by_role('button',name='刷新状态',exact=True).click()
                expect(page.locator('#p-node-counts')).to_have_text('业务已验证 0 · 连接通过 0 · 待检查 0 · 暂不可用 2')
                context.request.post(control+'/node',data={'state':'online'})
                page.get_by_role('button',name='刷新状态',exact=True).click()
                expect(page.locator('#p-node-counts')).to_have_text('业务已验证 0 · 连接通过 0 · 待检查 1 · 暂不可用 1')
                page.locator('.p-node-row').first.get_by_role('button',name='查看详情',exact=True).click()
                expect(page.locator('#p-proof-node')).to_have_text('在线')
                expect(page.locator('#p-proof-business')).to_have_text('尚未验证')
                expect(page.locator('#p-dialog-code')).not_to_have_attribute('open','')
                page.get_by_text('显示节点二维码',exact=True).click()
                expect(page.locator('#p-dialog-qr')).to_be_visible()
                fits(page)
                if registration:page.screenshot(path=str(artifacts/f'user-node-detail-{engine}-{width}-{theme}.png'),full_page=True)
                page.get_by_role('button',name='关闭节点详情',exact=True).click()
                if registration:
                    page.screenshot(path=str(artifacts / f'user-nodes-{engine}-{width}-{theme}.png'), full_page=True)
                for view in ['orders', 'account', 'help', 'preferences']:
                    page.evaluate('(view) => location.hash = view', view)
                    expect(page.locator(f'[data-view="{view}"]')).to_be_visible()
                    fits(page)
                page.evaluate('location.hash = "account"')
                expect(page.locator('#p-sessions-list')).to_contain_text('当前设备')
                page.once('dialog',lambda dialog:dialog.accept())
                page.get_by_role('button',name='退出其他设备',exact=True).click()
                expect(page.get_by_role('button',name='退出其他设备',exact=True)).to_be_disabled()
                assert other_context.request.get(base+'/u/sessions').status==401
                assert context.request.get(base+'/u/sessions').status==200
                token = next(c['value'] for c in context.cookies() if c['name']=='polaris_u')
                other_context.close()
                page.locator('.p-account-links a[href="#orders"]').click()
                expect(page.locator('[data-view="orders"]')).to_be_visible()
                expect(page.locator('.p-order-row')).to_have_count(1)
                expect(page.locator('.p-order-row')).to_contain_text('专业版月付')
                assert context.request.get(base+'/u/shop/order/foreign-order').status == 404
                page.evaluate('location.hash = "account"')
                if width > 760:
                    logout = page.locator('.p-sidebar-logout')
                else:
                    page.get_by_role('button', name='账号菜单', exact=True).click()
                    logout = page.locator('.p-user-menu-panel [data-portal-logout]')
                # A failed logout retains the session and permits retry.
                page.route('**/u/logout', lambda route: route.fulfill(status=503, json={'error':'temporary'}))
                logout.click()
                expect(page.locator('#p-session-error')).to_be_visible()
                expect(logout).to_be_enabled()
                assert context.request.get(base+'/u/sessions').status==200
                page.unroute('**/u/logout')
                logout.click()
                expect(page.locator('#mgo')).to_be_visible()
                replay = browser.new_context()
                replay.add_cookies([dict(name='polaris_u',value=token,domain=urlsplit(base).hostname,path='/u',httpOnly=True,sameSite='Lax')])
                assert replay.request.get(base+'/u/sessions').status==401, 'copied cookie survived logout'
                replay.close()
                assert not errors, errors
                context.close()
                print(engine, width, theme, 'registration gate, user-only data, navigation and filters OK', flush=True)
        browser.close()
