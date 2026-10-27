# Role-specific navigation and data access. APIs are mocked; backend role,
# projection, session and scope checks have separate integration tests.
import json, os, re, shutil
from pathlib import Path
from urllib.parse import urlparse
from playwright.sync_api import sync_playwright, expect

source=Path(__file__).with_name('settings-workflow-smoke.py')
scope={'__file__':str(source)}
exec(source.read_text().split('\nwith sync_playwright() as p:')[0],scope)
BaseFixture=scope['Fixture']
BASE=os.environ.get('POLARIS_WEB_TEST_URL','http://127.0.0.1:5187').rstrip('/')

class Fixture(BaseFixture):
    def __init__(self,role):
        super().__init__()
        self.role=role
        self.paths=[]
        self.created=None
    def route(self,route):
        path=urlparse(route.request.url).path.removeprefix('/api/v1')
        self.paths.append(path)
        result=None
        if path=='/auth/me': result=dict(id='owner' if self.role=='owner' else 'staff',username='fixture',role=self.role,assignedNodeIds=['n1'])
        elif path=='/admins':
            if route.request.method=='POST':
                self.created=route.request.post_data_json
                result=dict(id='new',**{k:v for k,v in self.created.items() if k!='password'})
            else: result=[dict(id='owner',username='fixture',role='owner',nodeIds=[])]
        elif path=='/staff/nodes':result=[dict(id='n1',name='Assigned server',status='online',cpu=12,mem=30,lastSeen='2026-10-27T01:00:00Z')]
        elif path=='/staff/users':result=[dict(id='u1',username='Customer fixture',status='active',usedUp=100,usedDown=200,trafficLimit=10000)]
        elif path=='/staff/orders':result=[dict(id='o1',planName='Plan fixture',amount='12.00',currency='USD',status='paid',method='manual',createdAt='2026-10-27T01:00:00Z')]
        elif path=='/staff/costs':result=[]
        elif path=='/tickets':result=dict(items=[],counts={},enabled=True)
        if result is not None:return route.fulfill(content_type='application/json',body=json.dumps(result))
        return super().route(route)

with sync_playwright() as p:
    for engine in os.environ.get('POLARIS_SETTINGS_BROWSERS','chromium,webkit').split(','):
        browser=p.chromium.launch(executable_path=os.environ.get('POLARIS_CHROMIUM_BIN') or shutil.which('chromium'),args=['--no-sandbox','--disable-dev-shm-usage']) if engine=='chromium' else p.webkit.launch()
        for width in [390,1440]:
            for lang in ['zh','en']:
                for role in ['viewer','support','finance','owner','operator']:
                    f=Fixture(role)
                    page=browser.new_page(viewport=dict(width=width,height=1000),is_mobile=width<760,has_touch=width<760,locale='zh-CN' if lang=='zh' else 'en-US')
                    page.add_init_script("localStorage.setItem('polaris.token','fixture');localStorage.setItem('polaris.refresh','0');localStorage.setItem('polaris.lang',%s)"%json.dumps(lang))
                    page.route('**/api/v1/**',f.route)
                    errors=[];page.on('pageerror',lambda e:errors.append(str(e)))
                    page.goto(BASE+'/settings#security')
                    if role=='owner':
                        page.get_by_role('button',name='添加后台账号' if lang=='zh' else 'Add administrator account',exact=True).click()
                        dialog=page.get_by_role('dialog')
                        dialog.locator('input').nth(0).fill('new-staff')
                        dialog.locator('input[type=password]').fill('synthetic-staff-password')
                        dialog.locator('select').select_option('support')
                        dialog.get_by_role('button',name='保存' if lang=='zh' else 'Save',exact=True).click()
                        expect(dialog).to_have_count(0)
                        assert f.created and f.created['role']=='support' and f.created['nodeIds']==[]
                    elif role=='operator':
                        expect(page.get_by_role('heading',name='我的节点' if lang=='zh' else 'My nodes',exact=True)).to_be_visible()
                        expect(page.get_by_role('button',name='添加节点' if lang=='zh' else 'Add node',exact=True)).to_have_count(0)
                        if width < 760: page.get_by_role('button',name='打开菜单' if lang=='zh' else 'Open menu',exact=True).click()
                        page.get_by_role('link',name='我的账号' if lang=='zh' else 'My account',exact=True).click()
                        expect(page.get_by_role('heading',name='修改密码' if lang=='zh' else 'Change password',exact=True)).to_be_visible()
                    else:
                        target={'viewer':'Assigned server','support':'Customer fixture','finance':'Plan fixture'}[role]
                        expect(page.get_by_text(target,exact=True)).to_be_visible()
                        if role=='support': expect(page.get_by_role('heading',name='工单' if lang=='zh' else 'Tickets',exact=True)).to_be_visible()
                        assert not any(path in f.paths for path in ['/nodes','/ws','/settings','/admins','/users','/orders','/tokens']),f.paths
                        expect(page.get_by_role('button',name='工单设置' if lang=='zh' else 'Ticket settings',exact=True)).to_have_count(0)
                        page.get_by_role('button',name='我的账号' if lang=='zh' else 'My account',exact=True).click()
                        expect(page.get_by_role('heading',name='修改密码' if lang=='zh' else 'Change password',exact=True)).to_be_visible()
                    assert page.evaluate('document.documentElement.scrollWidth <= innerWidth + 1'),'page overflow'
                    assert not errors,errors
                    page.close()
                print(engine,width,lang,'owner creation and restricted workspace projections passed',flush=True)
        browser.close()
