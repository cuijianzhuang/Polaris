# Settings navigation, failed writes, duplicate submissions and mobile layout.
# Mock APIs only. Run after starting Vite; CI runs Chromium and WebKit.
import json, os, re, shutil
from pathlib import Path
from urllib.parse import urlparse
from playwright.sync_api import sync_playwright, expect

source = Path(__file__).with_name('appearance-ui-smoke.py')
scope = {'__file__': str(source)}
exec(source.read_text().split('\nwith sync_playwright() as p:')[0], scope)
BaseFixture = scope['Fixture']
BASE = os.environ.get('POLARIS_WEB_TEST_URL', 'http://127.0.0.1:5187').rstrip('/')
OUT = Path(os.environ.get('POLARIS_SETTINGS_ARTIFACTS', '/tmp/polaris-settings-workflow'))
OUT.mkdir(parents=True, exist_ok=True)

class Fixture(BaseFixture):
    def __init__(self):
        super().__init__()
        self.settings = dict(smtp_host='smtp.example.test',smtp_port='587',smtp_security='starttls',smtp_user='fixture',smtp_from='fixture@example.test',smtp_from_name='Fixture',mail_notify_users='true',mail_notify_admin='true',mail_admin_to='',backup_schedule='off',backup_hour='4',backup_keep='7',auto_update='notify',auto_update_hour='4')
        self.fail_save = False
        self.hold_save = False
        self.pending_save = None
        self.save_count = 0
    def route(self, route):
        path = urlparse(route.request.url).path.removeprefix('/panel')
        if path == '/api/v1/settings':
            if route.request.method == 'PUT':
                self.save_count += 1
                body = route.request.post_data_json
                if self.fail_save:
                    return route.fulfill(status=503,content_type='application/json',body=json.dumps(dict(error='Synthetic save failure')))
                if self.hold_save:
                    self.pending_save = (route, body)
                    return
                self.settings.update(body)
            return route.fulfill(content_type='application/json',body=json.dumps(self.settings))
        extra = {
            '/api/v1/operational-health':dict(issues=[],states=[]),
            '/api/v1/system/update':dict(current='1.0',latest='1.0',newer=False,updater=True),
            '/api/v1/cluster':dict(members=[],operations=[],deliveries=[],shared=False),
            '/api/v1/security':dict(allowlist=self.settings.get('login_allowlist',''),banFailures=int(self.settings.get('login_ban_failures','10')),bans=[],trustedProxies=[],yourIp='127.0.0.1'),
            '/api/v1/sso/providers':dict(providers=[],callbackUrl=''),
            '/api/v1/auth/passkeys':dict(available=False,reason='Fixture',items=[],recoveryCodesLeft=0),
            '/api/v1/auth/github':dict(enabled=False,clientId='',callbackUrl=''),
        }
        if path in extra:
            return route.fulfill(content_type='application/json',body=json.dumps(extra[path]))
        if path == '/api/v1/backup/drills':
            return route.fulfill(content_type='application/json',body='[]')
        if path == '/api/v1/backup':
            return route.fulfill(content_type='application/json',body=json.dumps(dict(supported=True,db='sqlite',dbSize=10,dir='/fixture',encrypted=False,targets=[],files=[])))
        return super().route(route)
    def release(self):
        route, body = self.pending_save
        self.settings.update(body)
        self.pending_save = None
        route.fulfill(content_type='application/json',body='{}')

with sync_playwright() as p:
    for engine in os.environ.get('POLARIS_SETTINGS_BROWSERS','chromium,webkit').split(','):
        browser = (p.chromium.launch(executable_path=os.environ.get('POLARIS_CHROMIUM_BIN') or shutil.which('chromium'),args=['--no-sandbox','--disable-dev-shm-usage']) if engine=='chromium' else p.webkit.launch())
        for width in [int(v) for v in os.environ.get('POLARIS_SETTINGS_WIDTHS','390,1440').split(',') if v]:
            for language in os.environ.get('POLARIS_SETTINGS_LANGUAGES','zh,en').split(','):
                f = Fixture()
                page = browser.new_page(viewport=dict(width=width,height=900),is_mobile=width<760,has_touch=width<760,locale='zh-CN' if language=='zh' else 'en-US')
                page.set_default_timeout(10000)
                page.add_init_script("localStorage.setItem('polaris.token','fixture');localStorage.setItem('polaris.refresh','0');localStorage.setItem('polaris.lang',%s)" % json.dumps(language))
                page.route('**/api/v1/**',f.route)
                errors=[]
                page.on('pageerror',lambda e: errors.append(str(e)))
                page.goto(BASE+'/settings#mail')
                host=page.get_by_placeholder('smtp.example.com',exact=True)
                secret=page.locator('input[type=password]')
                save=page.locator('.settings-savebar button')
                nav=page.get_by_role('navigation',name='设置分区' if language=='zh' else 'Settings sections',exact=True)
                def tab(zh,en):
                    nav.get_by_role('button',name=re.compile('^'+(zh if language=='zh' else en))).click()
                def modal():
                    return page.get_by_role('dialog',name='有尚未保存的更改' if language=='zh' else 'Unsaved changes',exact=True)
                def decision(zh,en):
                    modal().get_by_role('button',name=zh if language=='zh' else en,exact=True).click()
                    if en != 'Save and leave': expect(modal()).to_be_hidden()
                def command(zh,en):
                    current=urlparse(page.url).path
                    if current.endswith('/nodes'):
                        expect(page.get_by_role('heading',name='节点与内核' if language=='zh' else 'Nodes & cores',exact=True)).to_be_visible()
                    page.evaluate('new Promise(resolve=>requestAnimationFrame(()=>requestAnimationFrame(resolve)))')
                    page.keyboard.press('Control+k')
                    page.get_by_role('dialog',name='快速跳转' if language=='zh' else 'Quick jump',exact=True).get_by_role('button',name=zh if language=='zh' else en,exact=True).click()
                expect(host).to_have_value('smtp.example.test')
                expect(save).to_be_disabled()
                host.fill('smtp.unsaved.test'); secret.fill('synthetic-secret')
                tab('通知策略','Notification policy')
                expect(modal()).to_be_visible()
                expect(modal().get_by_role('button',name='继续编辑' if language=='zh' else 'Continue editing',exact=True)).to_be_focused()
                modal().get_by_role('button').last.focus(); page.keyboard.press('Tab')
                expect(modal().get_by_role('button').first).to_be_focused()
                decision('继续编辑','Continue editing')
                expect(modal()).to_be_hidden(); expect(host).to_have_value('smtp.unsaved.test'); expect(secret).to_have_value('synthetic-secret')
                tab('通知策略','Notification policy'); decision('放弃修改','Discard changes')
                expect(page).to_have_url(BASE+'/settings#notifications')
                tab('邮件','Mail')
                expect(host).to_have_value('smtp.example.test'); expect(secret).to_have_value('')
                assert f.save_count==0
                host.fill('smtp.retry.test'); secret.fill('synthetic-secret')
                f.fail_save=True; save.click()
                expect(page.locator('.settings-savebar [role=alert]')).to_have_text('Synthetic save failure')
                expect(host).to_have_value('smtp.retry.test'); expect(secret).to_have_value('synthetic-secret')
                # Invalid ports cannot be submitted or saved by the leave dialog.
                port=page.locator('.field').filter(has=page.get_by_text('端口' if language=='zh' else 'Port',exact=True)).locator('input')
                port.fill('70000'); expect(save).to_be_disabled()
                tab('外观','Appearance'); expect(modal().get_by_role('button',name='保存后离开' if language=='zh' else 'Save and leave',exact=True)).to_be_disabled()
                decision('继续编辑','Continue editing'); port.fill('587')
                f.fail_save=False; f.hold_save=True; save.click()
                expect(save).to_be_disabled()
                before=f.save_count
                save.evaluate('(e)=>{e.click();e.click()}')
                assert f.save_count==before==2
                assert f.pending_save
                f.release(); f.hold_save=False
                expect(save).to_be_disabled(); expect(secret).to_have_value('')
                expect(page.locator('.settings-savebar [role=alert]')).to_have_count(0)
                # Failure in save-and-leave retains both the route and the draft.
                host.fill('smtp.leave.test'); f.fail_save=True
                tab('外观','Appearance'); decision('保存后离开','Save and leave')
                expect(modal().get_by_role('alert')).to_be_visible()
                modal().focus();page.keyboard.press('Shift+Tab')
                expect(modal().get_by_role('button').last).to_be_focused()
                expect(page).to_have_url(BASE+'/settings#mail')
                f.fail_save=False; f.hold_save=True
                decision('保存后离开','Save and leave')
                expect(modal().get_by_role('button',name='继续编辑' if language=='zh' else 'Continue editing',exact=True)).to_be_disabled()
                page.keyboard.press('Escape'); expect(modal()).to_be_visible()
                assert f.pending_save
                f.release(); f.hold_save=False
                expect(page).to_have_url(BASE+'/settings#appearance')
                tab('邮件','Mail'); expect(host).to_have_value('smtp.leave.test')
                # Commands and browser Back use the same guard as section tabs.
                host.fill('smtp.command.test'); command('节点列表','Node list')
                decision('继续编辑','Continue editing'); expect(host).to_have_value('smtp.command.test')
                command('节点列表','Node list'); decision('放弃修改','Discard changes')
                expect(page).to_have_url(BASE+'/nodes')
                command('设置','Settings'); tab('邮件','Mail')
                expect(host).to_have_value('smtp.leave.test'); host.fill('smtp.back.test')
                history_index=page.evaluate('history.state.idx')
                page.evaluate('history.back()'); expect(modal()).to_be_visible()
                decision('继续编辑','Continue editing'); expect(page).to_have_url(BASE+'/settings#mail')
                page.wait_for_function('(idx)=>history.state.idx===idx',arg=history_index)
                page.evaluate('new Promise(resolve=>requestAnimationFrame(()=>requestAnimationFrame(resolve)))')
                page.evaluate('history.back()'); decision('放弃修改','Discard changes')
                expect(page).to_have_url(BASE+'/nodes')
                command('设置','Settings'); tab('邮件','Mail')
                host.fill('smtp.refresh.test')
                assert page.evaluate("!window.dispatchEvent(new Event('beforeunload',{cancelable:true}))")
                stored=page.evaluate("JSON.stringify([localStorage,sessionStorage])")
                assert 'synthetic-secret' not in stored and 'smtp.refresh.test' not in stored
                if width<760:
                    # This emulates visual viewport events; a physical iOS keyboard still needs device testing.
                    host.focus()
                    page.evaluate("Object.defineProperty(visualViewport,'height',{configurable:true,value:450});visualViewport.dispatchEvent(new Event('resize'))")
                    expect(page.locator('html')).to_have_attribute('data-keyboard','')
                    expect(page.locator('.mtabs')).to_be_hidden()
                    assert page.locator('.settings-savebar').bounding_box()['y'] + page.locator('.settings-savebar').bounding_box()['height'] <= 451
                    host.blur()
                    expect(page.locator('html')).not_to_have_attribute('data-keyboard','')
                    page.evaluate("delete visualViewport.height;visualViewport.dispatchEvent(new Event('resize'))")
                tab('常规','General'); decision('放弃修改','Discard changes')
                advanced=page.locator('details.settings-disclosure')
                expect(advanced).to_have_count(3)
                for item in advanced.all(): assert not item.evaluate('(e)=>e.open')
                assert page.evaluate('document.documentElement.scrollWidth<=innerWidth+1')
                assert not errors, errors
                page.screenshot(path=str(OUT/f'settings-{engine}-{width}-{language}.png'),animations='disabled')
                page.close()
                print(engine,width,language,'draft decisions, failed saves, duplicate lock, commands, Back and mobile viewport passed',flush=True)
        # Shared guard/save behavior on the other persistent settings forms.
        f=Fixture();page=browser.new_page(viewport=dict(width=390,height=900),is_mobile=True,has_touch=True)
        secondary_errors=[];page.on('pageerror',lambda e: secondary_errors.append(str(e)))
        page.add_init_script("localStorage.setItem('polaris.token','fixture');localStorage.setItem('polaris.lang','zh');localStorage.setItem('polaris.refresh','0')")
        page.route('**/api/v1/**',f.route)
        page.goto(BASE+'/settings#notifications')
        def section(name): page.get_by_role('navigation',name='设置分区',exact=True).get_by_role('button',name=re.compile('^'+name)).click()
        def field(name): return page.locator('.field').filter(has=page.get_by_text(name,exact=True)).locator('input, select, textarea').first
        def save_leave(): page.get_by_role('dialog',name='有尚未保存的更改',exact=True).get_by_role('button',name='保存后离开',exact=True).click()
        page.get_by_text('告警合并与维护静默',exact=True).click()
        page.get_by_role('spinbutton',name='重复告警合并窗口（秒）',exact=True).fill('600')
        section('邮件');save_leave();expect(page).to_have_url(BASE+'/settings#mail')
        assert f.notification['policy']['repeatWindowSeconds']==600
        section('备份与恢复');field('自动备份').select_option('daily')
        backup_save=page.get_by_role('button',name='保存设置',exact=True)
        f.fail_save=True;backup_save.click()
        expect(page.locator('.settings-savebar [role=alert]')).to_be_visible()
        expect(field('自动备份')).to_have_value('daily')
        f.fail_save=False;backup_save.click()
        expect(backup_save).to_be_disabled()
        expect(page.locator('.settings-savebar [role=alert]')).to_have_count(0)
        assert f.settings['backup_schedule']=='daily'
        field('自动备份').select_option('weekly')
        section('邮件');save_leave();expect(page).to_have_url(BASE+'/settings#mail')
        assert f.settings['backup_schedule']=='weekly'
        section('常规');field('站点标题').fill('Workflow fixture')
        page.get_by_placeholder('/',exact=True).fill('/ops')
        section('邮件')
        expect(page.get_by_role('dialog').get_by_role('button',name='保存后离开',exact=True)).to_be_disabled()
        page.get_by_role('dialog').get_by_role('button',name='继续编辑',exact=True).click()
        page.get_by_placeholder('/',exact=True).fill('/')
        section('邮件');save_leave();expect(page).to_have_url(BASE+'/settings#mail')
        assert f.settings['site_title']=='Workflow fixture'
        section('版本与更新');field('自动更新').select_option('panel')
        expect(field('自动更新')).to_have_value('panel')
        expect(page.locator('.settings-savebar button')).to_be_enabled()
        section('邮件');save_leave();expect(page).to_have_url(BASE+'/settings#mail')
        assert f.settings['auto_update']=='panel'
        section('账号安全');field('封禁阈值（次）').fill('25')
        section('邮件');save_leave();expect(page).to_have_url(BASE+'/settings#mail')
        assert f.settings['login_ban_failures']=='25'
        assert not secondary_errors,secondary_errors
        page.close()
        print(engine,'notification, backup, general, update and login-protection saves passed',flush=True)
        page=browser.new_page(viewport=dict(width=390,height=900))
        page.add_init_script("window.__POLARIS_BASE__='/panel/';localStorage.setItem('polaris.token','fixture');localStorage.setItem('polaris.lang','zh');localStorage.setItem('polaris.refresh','0')")
        def prefixed(route):
            path=urlparse(route.request.url).path.removeprefix('/panel/api/v1')
            data={'/auth/me':dict(username='admin'),'/info':dict(title='Polaris',edition='community'),'/license':dict(edition='community',state=dict(features=[],limits={},warnings=[]),usage={}),'/settings':Fixture().settings}.get(path,[])
            route.fulfill(content_type='application/json',body=json.dumps(data))
        page.route('**/api/v1/**',prefixed)
        page.goto(BASE+'/panel/settings#mail')
        page.get_by_placeholder('smtp.example.com',exact=True).fill('smtp.prefix.test')
        page.keyboard.press('Control+k')
        page.get_by_role('dialog',name='快速跳转',exact=True).get_by_role('button',name='节点列表',exact=True).click()
        page.get_by_role('dialog',name='有尚未保存的更改',exact=True).get_by_role('button',name='放弃修改',exact=True).click()
        expect(page).to_have_url(BASE+'/panel/nodes')
        page.close()
        print(engine,'prefixed admin navigation and draft guard passed',flush=True)
        browser.close()
