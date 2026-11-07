"""Real user portal navigation, registration gates, ownership and responsive UI."""
import json, os, shutil
from pathlib import Path
from urllib.parse import urlsplit
from playwright.sync_api import sync_playwright, expect

base = os.environ['POLARIS_PORTAL_TEST_URL']
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
        expect(page.locator('[data-view="overview"]')).to_be_visible()
        if registration:
            expect(page.locator('.p-subscription')).to_contain_text('待开通')
            expect(page.locator('.p-subscription')).not_to_contain_text('不限流量')
        assert context.request.get(base + os.environ['POLARIS_PORTAL_ADMIN_API'] + '/users').status == 401
        context.close()

        for width in [360, 1440]:
            for theme in ['light', 'dark']:
                context = browser.new_context(viewport=dict(width=width, height=1000), color_scheme=theme)
                context.add_cookies([dict(name='polaris_u',value=os.environ['POLARIS_PORTAL_TEST_COOKIE'],domain=urlsplit(base).hostname,path='/u',httpOnly=True,sameSite='Lax')])
                page = context.new_page()
                page.set_default_timeout(10000)
                page.on('pageerror', lambda error: errors.append(str(error)))
                page.goto(base + '/u')
                expect(page.locator('[data-view="overview"]')).to_be_visible()
                original = page.locator('.p-heading h1').first.inner_text()
                page.locator('.p-heading h1').first.evaluate('(el) => el.textContent = "欢迎回来，" + "longusername".repeat(3)')
                fits(page)
                page.locator('.p-heading h1').first.evaluate('(el, text) => el.textContent = text', original)
                expect(page.get_by_role('heading', name='欢迎回来，alice')).to_be_visible()
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
                expect(page.locator('#p-node-counts')).to_have_text('已验证 0 · 未验证 1 · 暂不可用 1')
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
                if registration:
                    page.screenshot(path=str(artifacts / f'user-nodes-{engine}-{width}-{theme}.png'), full_page=True)
                for view in ['orders', 'account', 'help', 'preferences']:
                    page.evaluate('(view) => location.hash = view', view)
                    expect(page.locator(f'[data-view="{view}"]')).to_be_visible()
                    fits(page)
                page.evaluate('location.hash = "account"')
                page.locator('.p-account-links a[href="#orders"]').click()
                expect(page.locator('[data-view="orders"]')).to_be_visible()
                expect(page.locator('.p-order-row')).to_have_count(1)
                expect(page.locator('.p-order-row')).to_contain_text('专业版月付')
                assert context.request.get(base+'/u/shop/order/foreign-order').status == 404
                page.evaluate('location.hash = "account"')
                page.get_by_role('button', name='退出登录', exact=True).click()
                expect(page.locator('#mgo')).to_be_visible()
                assert not errors, errors
                context.close()
                print(engine, width, theme, 'registration gate, user-only data, navigation and filters OK', flush=True)
        browser.close()
