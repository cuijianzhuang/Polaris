"""Existing-account linking through real portal, SMTP, cookies and SSR/JS."""
import os, json, shutil, re
from pathlib import Path
from playwright.sync_api import sync_playwright, expect

base=os.environ['POLARIS_PORTAL_TEST_URL']
control=os.environ['POLARIS_PORTAL_TEST_CONTROL']
artifacts=Path(os.environ.get('POLARIS_BINDING_ARTIFACTS','/tmp/polaris-account-binding'))
artifacts.mkdir(parents=True,exist_ok=True)

def fits(page,dialog):
    assert page.evaluate('document.documentElement.scrollWidth<=innerWidth+1')
    assert dialog.evaluate('(d)=>d.scrollWidth<=d.clientWidth+1')
    assert dialog.bounding_box()['x']>=0

with sync_playwright() as p:
    for engine in os.environ.get('POLARIS_TEST_ENGINES','chromium,webkit').split(','):
        options=dict(headless=True)
        if engine=='chromium':options.update(executable_path=os.environ.get('POLARIS_CHROMIUM_BIN')or shutil.which('chromium'),args=['--no-sandbox','--disable-dev-shm-usage'])
        browser=getattr(p,engine).launch(**options)
        for width in [320,390,1440]:
            for mode in ['subscription','email','conflict','telegram']:
                fixture=p.request.new_context().post(control+'/fixture',data=dict(mode=mode)).json()
                context=browser.new_context(viewport=dict(width=width,height=900))
                context.add_init_script("localStorage.setItem('polaris.portal.autoCheck','false')")
                page=context.new_page();errors=[]
                page.on('pageerror',lambda error:errors.append(str(error)))
                page.route('https://www.gravatar.com/**',lambda route:route.abort())
                if mode=='subscription':
                    page.goto(fixture['subscription'])
                    page.get_by_role('link',name='关联已有账号',exact=True).click()
                    expect(page.locator('#memail')).to_be_visible()
                    expect(page.get_by_text('登录已有账号后即可关联这条订阅，无需重复注册。')).to_be_visible()
                    assert 'bind-subscription' not in page.url
                    page.locator('#memail').fill(fixture['email']);page.locator('#mcode').fill(fixture['loginCode'])
                    page.locator('#mgo').click()
                    dialog=page.locator('#p-subscription-binding');expect(dialog).to_be_visible()
                    expect(page.locator('#p-binding-link')).to_have_value(fixture['subToken'])
                    expect(page.locator('#p-binding-confirm')).to_be_hidden()
                    fits(page,dialog)
                    page.locator('#p-binding-check').click();expect(page.locator('#p-binding-preview')).to_contain_text(fixture['username'])
                    expect(page.locator('#p-binding-confirm')).to_be_visible()
                    page.locator('#p-binding-link').fill(fixture['subscription'])
                    expect(page.locator('#p-binding-confirm')).to_be_hidden()
                    page.locator('#p-binding-check').click();expect(page.locator('#p-binding-confirm')).to_be_visible()
                    fits(page,dialog)
                    if width==390:page.screenshot(path=str(artifacts/(engine+'-subscription.png')))
                    page.locator('#p-binding-confirm').click();expect(dialog).to_be_hidden()
                    expect(page.locator('[data-view="account"]')).to_contain_text(fixture['email'])
                    assert fixture['subToken'] in page.content()
                    assert page.evaluate("sessionStorage.getItem('polaris.pendingSubscription')")is None
                else:
                    context.add_cookies([dict(name='polaris_u',value=fixture['cookie'],url=base+'/u',httpOnly=True,sameSite='Lax')])
                    page.goto(base+'/u#account')
                    if mode=='telegram':
                        button=page.locator('#p-bind-telegram');button.click();dialog=page.locator('#p-telegram-binding');expect(dialog).to_be_visible()
                        expect(page.locator('#p-binding-tg-number')).to_have_text(re.compile(r'^\d{2}$'))
                        with page.expect_response(lambda r:'/telegram/bind/cancel' in r.url) as cancelled:
                            dialog.get_by_role('button',name='取消',exact=True).click()
                        assert cancelled.value.status==200
                        expect(dialog).to_be_hidden()
                        with page.expect_response(lambda r:'/telegram/bind/start' in r.url) as started:button.click()
                        challenge=started.value.json()
                        expect(page.locator('#p-binding-tg-number')).to_have_text(str(challenge['code']))
                        assert challenge['secret'] not in page.locator('#p-binding-tg-open').get_attribute('href')
                        fits(page,dialog)
                        if width==390:page.screenshot(path=str(artifacts/(engine+'-telegram.png')))
                        response=p.request.new_context().post(control+'/answer',data=dict(id=challenge['id'],pick=str(challenge['code']),chat=fixture['chat']))
                        assert response.ok,response.text()
                        expect(dialog).to_be_hidden(timeout=15000)
                        expect(page.locator('[data-view="account"]')).to_contain_text(fixture['email'])
                        expect(page.locator('#p-account-binding')).to_contain_text('已绑定')
                        assert fixture['subToken'] in page.content()
                    elif mode=='conflict':

                        page.locator('#p-bind-subscription').click();dialog=page.locator('#p-subscription-binding')
                        page.locator('#p-binding-link').fill(fixture['subscription']);page.locator('#p-binding-check').click()
                        expect(page.locator('#p-binding-error')).to_contain_text('不能直接关联')
                        expect(page.locator('#p-binding-confirm')).to_be_hidden();fits(page,dialog)
                        page.keyboard.press('Escape');expect(dialog).to_be_hidden()
                        assert fixture['oldToken'] in page.content()
                    else:
                        page.locator('[data-portal="email"]').first.click();dialog=page.locator('#p-email-binding');expect(dialog).to_be_visible()
                        page.locator('#p-binding-email').fill(fixture['email']);page.locator('#p-binding-send').click()
                        expect(page.locator('#p-binding-mail-note')).to_contain_text('验证码已发送')
                        code=p.request.new_context().post(control+'/code',data=dict(email=fixture['email'])).json()['code']
                        page.locator('#p-binding-code').fill('000000' if code!='000000' else '999999')
                        page.locator('#p-binding-mail-confirm').click();expect(page.locator('#p-binding-mail-error')).to_contain_text('验证码不正确')
                        fits(page,dialog)
                        if width==390:page.screenshot(path=str(artifacts/(engine+'-email.png')))
                        page.locator('#p-binding-code').fill(code);page.locator('#p-binding-mail-confirm').click()
                        expect(dialog).to_be_hidden();expect(page.locator('[data-view="account"]')).to_contain_text(fixture['email'])
                        assert fixture['subToken'] in page.content()
                assert not errors,errors
                assert page.evaluate('document.documentElement.scrollWidth<=innerWidth+1')
                context.close();print(engine,width,mode,'passed',flush=True)
        browser.close()
