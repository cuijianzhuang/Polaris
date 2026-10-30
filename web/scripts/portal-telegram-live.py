"""Execute the real /u login page and handlers; never contact Telegram.
Invoked by TestLivePortalTelegramBrowser, with and without e-mail sign-in.
"""
import os
import shutil
from playwright.sync_api import sync_playwright, expect

base = os.environ['POLARIS_PORTAL_TEST_URL']
mail = os.environ['POLARIS_PORTAL_EXPECT_MAIL'] == 'true'
with sync_playwright() as p:
    for engine in ['chromium', 'webkit']:
        options = dict(headless=True)
        if engine == 'chromium':
            options.update(executable_path=os.environ.get('POLARIS_CHROMIUM_BIN') or shutil.which('chromium'),
                           args=['--no-sandbox', '--disable-dev-shm-usage'])
        browser = getattr(p, engine).launch(**options)
        for width in [390, 1440]:
            context = browser.new_context(viewport=dict(width=width, height=1000))
            context.route('https://t.me/**', lambda route: route.fulfill(body='Telegram test placeholder'))
            page = context.new_page()
            page.set_default_timeout(10000)
            errors = []
            page.on('pageerror', lambda e: errors.append(str(e)))
            page.goto(base + '/u')
            expect(page.get_by_role('button', name='发送验证码', exact=True)).to_have_count(1 if mail else 0)
            with page.expect_response(base + '/u/login') as started:
                page.get_by_role('button', name='使用 Telegram 登录', exact=True).click()
            response = started.value
            assert response.status == 200, f'login returned HTTP {response.status}'
            assert response.request.headers.get('x-polaris-portal') == '1'
            request = response.json()
            expect(page.locator('#num')).to_have_text(str(request['code']))
            expect(page.get_by_role('link', name='打开 Telegram', exact=True)).to_have_attribute('href', request['url'])
            assert request['url'].startswith('https://t.me/polaris_test_bot?start=ulogin_')
            with page.expect_response(base + '/u/poll') as polled:
                expect(page.locator('#state')).to_have_text('等待确认…')
            response = polled.value
            assert response.status == 200, f'poll returned HTTP {response.status}'
            assert response.json()['status'] == 'pending'
            assert response.request.headers.get('x-polaris-portal') == '1'
            assert response.request.post_data_json == dict(id=request['id'], secret=request['secret'])
            expect(page.locator('#err')).to_be_hidden()
            # The fix must not relax the backend's protection on either route.
            for path in ['login', 'poll']:
                denied = context.request.post(base + '/u/' + path, data=dict(id=request['id'], secret=request['secret']))
                assert denied.status == 403, f'{path} accepted a request without the CSRF header'
            assert not errors, errors
            context.close()
            print(engine, width, 'mail=' + str(mail), 'login + poll OK; missing CSRF header rejected')
        browser.close()
