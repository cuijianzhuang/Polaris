"""Real portal login, local bot confirmation, cancellation and failure recovery."""
import os, shutil
from pathlib import Path
from playwright.sync_api import sync_playwright, expect

base = os.environ['POLARIS_PORTAL_TEST_URL']
mail = os.environ['POLARIS_PORTAL_EXPECT_MAIL'] == 'true'
control = os.environ['POLARIS_TG_TEST_CONTROL']
out = Path(os.environ.get('POLARIS_TG_ARTIFACTS','/tmp/polaris-telegram-login'))
out.mkdir(parents=True,exist_ok=True)
headers = {'X-Polaris-Portal':'1'}
with sync_playwright() as p:
    for engine in ['chromium','webkit']:
        options = dict(headless=True)
        if engine == 'chromium':
            options.update(executable_path=os.environ.get('POLARIS_CHROMIUM_BIN') or shutil.which('chromium'),args=['--no-sandbox','--disable-dev-shm-usage'])
        browser = getattr(p,engine).launch(**options)
        for width in [390,1440]:
            context = browser.new_context(viewport=dict(width=width,height=1000),color_scheme='dark' if width==1440 else 'light')
            context.route('https://t.me/**',lambda route:route.fulfill(body='Telegram test placeholder'))
            page = context.new_page()
            page.set_default_timeout(10000)
            errors=[];page.on('pageerror',lambda e:errors.append(str(e)))
            page.goto(base+'/u/login')
            expect(page.get_by_role('button',name='发送验证码',exact=True)).to_have_count(1 if mail else 0)
            go=page.get_by_role('button',name='使用 Telegram 登录',exact=True)
            expect(go).to_be_enabled()
            # A reconnect error keeps the login method available for retry.
            page.route(base+'/u/login',lambda route:route.fulfill(status=503,json={'error':'Telegram 机器人暂时未就绪，请稍后重试或联系管理员'}))
            go.click();expect(page.locator('#err')).to_contain_text('暂时未就绪');expect(go).to_be_enabled()
            page.unroute(base+'/u/login')
            page.route(base+'/u/login',lambda route:route.abort())
            go.click();expect(page.locator('#err')).to_have_text('暂时无法连接，请稍后重试');expect(go).to_be_enabled()
            page.unroute(base+'/u/login')
            # Local expiry still ends an attempt when polling cannot reach the server.
            page.route(base+'/u/login',lambda route:route.fulfill(json={'id':'expired-fixture','secret':'browser-only-secret','code':37,'url':'https://t.me/polaris_bot?start=ulogin_fixture','expiresIn':1}))
            page.route(base+'/u/poll',lambda route:route.abort())
            go.click();expect(page.locator('#num')).to_have_text('37')
            expect(page.locator('#err')).to_contain_text('已过期');expect(go).to_be_enabled()
            page.unroute(base+'/u/login');page.unroute(base+'/u/poll')
            # Rejection restores the button and clears the old deep link.
            page.route(base+'/u/login',lambda route:route.fulfill(json={'id':'denied-fixture','secret':'browser-only-secret','code':38,'url':'https://t.me/polaris_bot?start=ulogin_fixture','expiresIn':180}))
            page.route(base+'/u/poll',lambda route:route.fulfill(json={'status':'denied'}))
            go.click();expect(page.locator('#err')).to_contain_text('已被拒绝');expect(go).to_be_enabled()
            expect(page.locator('#open')).to_have_attribute('href','#')
            page.unroute(base+'/u/login');page.unroute(base+'/u/poll')
            # Start a real request, preserving the CSRF protection and secret.
            with page.expect_response(base+'/u/login') as started:go.click()
            response=started.value;assert response.status==200
            assert response.request.headers.get('x-polaris-portal')=='1'
            request=response.json();credentials=dict(id=request['id'],secret=request['secret'])
            expect(page.locator('#num')).to_have_text(str(request['code']))
            expect(page.get_by_role('link',name='打开 Telegram',exact=True)).to_have_attribute('href',request['url'])
            assert request['url'].startswith('https://t.me/polaris_bot?start=ulogin_')
            expect(page.locator('#expires')).to_contain_text('内完成确认')
            if mail:expect(page.locator('#m1')).to_be_hidden()
            for path in ['login','poll','login/cancel']:
                assert context.request.post(base+'/u/'+path,data=credentials).status==403
            assert page.evaluate('document.documentElement.scrollWidth<=innerWidth+1')
            page.screenshot(path=str(out/f'{engine}-{width}-mail-{mail}.png'),full_page=True)
            if width==1440:
                # Unbound chats receive binding guidance, never a user session.
                context.request.post(control+'/test/answer',data={'ID':request['id'],'Chat':9999})
                with page.expect_response(base+'/u/poll') as polled:
                    expect(page.locator('#state')).to_contain_text('已绑定')
                assert polled.value.json()['status']=='pending'
                with page.expect_response(base+'/u/login/cancel') as cancelled:
                    if mail:
                        page.get_by_role('link',name='注册',exact=True).click()
                        expect(page.locator('#tg-area')).to_be_hidden()
                    else:
                        page.get_by_role('button',name='取消本次登录',exact=True).click()
                assert cancelled.value.status==200
                assert context.request.post(base+'/u/poll',data=credentials,headers=headers).json()['status']=='expired'
                # A late callback cannot resurrect a cancelled request.
                context.request.post(control+'/test/answer',data={'ID':request['id'],'Chat':701,'Pick':str(request['code'])})
                assert context.request.post(base+'/u/poll',data=credentials,headers=headers).json()['status']=='expired'
                if mail:
                    page.get_by_role('link',name='登录',exact=True).click()
                expect(go).to_be_enabled()
                if mail:expect(page.locator('#m1')).to_be_visible()
            else:
                context.request.post(control+'/test/answer',data={'ID':request['id'],'Chat':701})
                expect(page.locator('#state')).to_contain_text('已在 Telegram 打开')
                context.request.post(control+'/test/answer',data={'ID':request['id'],'Chat':701,'Pick':str(request['code'])})
                expect(page.locator('[data-view="overview"]')).to_be_visible()
                assert context.request.get(base+os.environ['POLARIS_PORTAL_ADMIN_API']+'/users').status==401
                assert context.request.post(base+'/u/poll',data=credentials,headers=headers).json()['status']=='expired'
            assert not errors,errors
            context.close()
            print(engine,width,'mail='+str(mail),'availability, expiry, rejection, bot confirmation/cancellation and isolation OK',flush=True)
        browser.close()
