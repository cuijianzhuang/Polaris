"""Draft preservation, optimistic conflicts and slow-network refresh scheduling."""
import copy,json,os,shutil,time
from pathlib import Path
from urllib.parse import urlparse
from playwright.sync_api import sync_playwright,expect

source=Path(__file__).with_name('settings-workflow-smoke.py')
scope={'__file__':str(source)}
exec(source.read_text().split('\ndef wait_routes(page, fixture, predicate):
    # Request events precede route callbacks. Pump the driver until the held
    # route exists, independently of the page's controlled timer clock.
    deadline=time.monotonic()+10
    while not predicate(fixture.user_requests):
        assert time.monotonic()<deadline,'expected held request was not routed'
        page.wait_for_timeout(10)

def close_page(page, fixture):
    for route in fixture.user_requests:
        try:route.abort()
        except Exception:pass # already fulfilled or cancelled by the browser
    for route,_ in fixture.pending_lists:
        try:route.abort()
        except Exception:pass
    page.unroute_all(behavior='wait');page.close()

with sync_playwright() as p:')[0],scope)
online_source=Path(__file__).with_name('user-online-ui-smoke.py')
online={'__file__':str(online_source)}
exec(online_source.read_text().split('\ndef wait_routes(page, fixture, predicate):
    # Request events precede route callbacks. Pump the driver until the held
    # route exists, independently of the page's controlled timer clock.
    deadline=time.monotonic()+10
    while not predicate(fixture.user_requests):
        assert time.monotonic()<deadline,'expected held request was not routed'
        page.wait_for_timeout(10)

def close_page(page, fixture):
    for route in fixture.user_requests:
        try:route.abort()
        except Exception:pass # already fulfilled or cancelled by the browser
    for route,_ in fixture.pending_lists:
        try:route.abort()
        except Exception:pass
    page.unroute_all(behavior='wait');page.close()

with sync_playwright() as p:')[0],online)
BASE=os.environ.get('POLARIS_WEB_TEST_URL','http://127.0.0.1:5187').rstrip('/')

class Fixture(scope['Fixture']):
    def __init__(self):
        super().__init__()
        self.templates=[dict(id='clash-a',format='clash',name='Clash primary',content='clash original',isDefault=True,revision=1,updatedAt='2026-11-07T00:00:00Z'),dict(id='clash-b',format='clash',name='Clash alternate',content='alternate original',isDefault=False,revision=1,updatedAt='2026-11-07T00:00:00Z'),dict(id='sing-a',format='singbox',name='Sing primary',content='sing original',isDefault=True,revision=1,updatedAt='2026-11-07T00:00:00Z')]
        self.settings['sub_name_template']='{node}'
        self.fail_template=False;self.hold_lists=False;self.pending_lists=[];self.template_writes=[]
        self.hold_users=False;self.user_requests=[]
        self.user=copy.deepcopy(online['Fixture']().users['known'])
    def route(self,route):
        req=route.request;path=urlparse(req.url).path.removeprefix('/api/v1')
        if path=='/sub-templates':
            if self.hold_lists:self.pending_lists.append((route,copy.deepcopy(self.templates)));return
            return route.fulfill(json=self.templates)
        if path.startswith('/sub-templates/') and req.method=='PUT':
            body=req.post_data_json;self.template_writes.append(body)
            if self.fail_template:return route.fulfill(status=503,json={'error':'synthetic template save failure'})
            item=next(row for row in self.templates if row['id']==path.split('/')[-1])
            if body.get('revision')!=item['revision']:return route.fulfill(status=409,json={'error':'synthetic concurrent template update','code':'template_conflict'})
            item.update({key:body[key] for key in ['content','name','isDefault'] if key in body});item['revision']+=1
            return route.fulfill(json=item)
        if path=='/users' and self.hold_users:
            self.user_requests.append(route)
            return
        return super().route(route)
    def users(self,name):
        return dict(items=[dict(self.user,username=name)],total=1,summary={'all':1,'active':1})

def configure(page,language='zh',refresh='0'):
    page.set_default_timeout(10000)
    page.add_init_script("localStorage.setItem('polaris.token','fixture');localStorage.setItem('polaris.lang',%s);localStorage.setItem('polaris.refresh',%s)"%(json.dumps(language),json.dumps(refresh)))

def wait_routes(page, fixture, predicate):
    # Request events precede route callbacks. Pump the driver until the held
    # route exists, independently of the page's controlled timer clock.
    deadline=time.monotonic()+10
    while not predicate(fixture.user_requests):
        assert time.monotonic()<deadline,'expected held request was not routed'
        page.wait_for_timeout(10)

def close_page(page, fixture):
    for route in fixture.user_requests:
        try:route.abort()
        except Exception:pass # already fulfilled or cancelled by the browser
    for route,_ in fixture.pending_lists:
        try:route.abort()
        except Exception:pass
    page.unroute_all(behavior='wait');page.close()

with sync_playwright() as p:
    for engine in os.environ.get('POLARIS_TEST_ENGINES','chromium,webkit').split(','):
        launch=dict(headless=True)
        if engine=='chromium':launch.update(executable_path=os.environ.get('POLARIS_CHROMIUM_BIN')or shutil.which('chromium'),args=['--no-sandbox','--disable-dev-shm-usage'])
        browser=getattr(p,engine).launch(**launch)
        for width,language in [(390,'zh'),(1440,'en')]:
            f=Fixture();page=browser.new_page(viewport=dict(width=width,height=1000));configure(page,language)
            page.route('**/api/v1/**',f.route)
            label=lambda zh,en:zh if language=='zh' else en
            page.goto(BASE+'/sub-templates')
            field=page.get_by_label(label('模板内容','Template content'),exact=True)
            expect(field).to_have_value('clash original')
            editor=page.locator('section.card').filter(has=field)
            save=editor.get_by_role('button',name=label('保存','Save'),exact=True)
            modal=page.get_by_role('dialog',name=label('有尚未保存的更改','Unsaved changes'),exact=True)
            other_tab=page.get_by_role('tab',name='sing-box / SFA / SFI',exact=True)
            field.fill('draft to save')
            page.get_by_role('button',name='Clash primary').click()
            expect(field).to_have_value('draft to save')
            other_tab.click()
            modal.get_by_role('button',name=label('继续编辑','Continue editing'),exact=True).click()
            expect(field).to_have_value('draft to save')
            f.fail_template=True;other_tab.click()
            modal.get_by_role('button',name=label('保存后离开','Save and leave'),exact=True).click()
            expect(modal).to_contain_text(label('保存失败','Save failed'))
            modal.get_by_role('button',name=label('继续编辑','Continue editing'),exact=True).click()
            expect(field).to_have_value('draft to save')
            f.fail_template=False;other_tab.click()
            modal.get_by_role('button',name=label('保存后离开','Save and leave'),exact=True).click()
            expect(field).to_have_value('sing original')
            assert f.templates[0]['content']=='draft to save'
            page.get_by_role('tab',name='Clash / mihomo / Stash',exact=True).click()
            expect(field).to_have_value('draft to save')
            field.fill('second saved draft');f.hold_lists=True
            with page.expect_request(lambda req:urlparse(req.url).path.endswith('/sub-templates')):save.click()
            field.fill('next draft during refresh')
            f.hold_lists=False
            with page.expect_response(lambda response:urlparse(response.url).path.endswith('/sub-templates')) as refreshed:
                for route,data in f.pending_lists:route.fulfill(json=data)
            refreshed.value.json()
            expect(field).to_have_value('next draft during refresh');expect(save).to_be_enabled()
            f.templates[0].update(content='external saved template',revision=f.templates[0]['revision']+1)
            save.click();expect(editor.get_by_role('alert')).to_contain_text('synthetic concurrent')
            expect(field).to_have_value('next draft during refresh')
            editor.get_by_role('button',name=label('重新加载服务器内容','Reload server content'),exact=True).click()
            page.get_by_role('dialog').get_by_role('button',name=label('确定','OK'),exact=True).click()
            expect(field).to_have_value('external saved template');expect(save).to_be_disabled()
            expect(editor.get_by_role('alert')).to_have_count(0)
            name_card=page.locator('section.card').filter(has=page.get_by_role('heading',name=label('节点名称模板','Node name template'),exact=True))
            name_field=name_card.locator('input');name_field.fill('unsaved-name')
            other_tab.click();expect(modal).to_contain_text(label('节点名称模板','Node name template'))
            modal.get_by_role('button',name=label('放弃修改','Discard changes'),exact=True).click()
            expect(name_field).to_have_value('{node}')
            close_page(page,f);print(engine,width,language,'draft switches, failed saves, late responses, conflicts and discard passed',flush=True)
        # A response slower than the refresh period must reach the page.
        f=Fixture();f.hold_users=True
        page=browser.new_page(viewport=dict(width=1440,height=900));configure(page,refresh='2000');page.clock.install()
        page.route('**/api/v1/**',f.route)
        page.goto(BASE+'/users')
        wait_routes(page,f,lambda routes:bool(routes))
        page.clock.run_for(1000) # settle the development StrictMode mount/abort
        initial_requests=len(f.user_requests)
        page.clock.run_for(6500)
        assert len(f.user_requests)==initial_requests,'polling overlapped a slow request'
        with page.expect_request(lambda req:urlparse(req.url).path.endswith('/users')):
            f.user_requests[-1].fulfill(json=f.users('slow response delivered'))
        expect(page.get_by_text('slow response delivered',exact=True)).to_be_visible()
        assert len(f.user_requests)==initial_requests+1,'refreshes were not coalesced into one follow-up'
        with page.expect_request(lambda req:'q=new-query' in req.url):
            page.locator('#q').fill('new-query');page.clock.run_for(350)
        wait_routes(page,f,lambda routes:'q=new-query' in routes[-1].request.url)
        f.user_requests[-1].fulfill(json=f.users('new query result'))
        expect(page.get_by_text('new query result',exact=True)).to_be_visible()
        close_page(page,f);print(engine,'slow responses, serialized refresh and query changes passed',flush=True)
        # The deadline releases a stalled request and leaves an explicit retry.
        f=Fixture();f.hold_users=True
        page=browser.new_page(viewport=dict(width=390,height=844));configure(page);page.clock.install();page.route('**/api/v1/**',f.route)
        page.goto(BASE+'/users')
        wait_routes(page,f,lambda routes:bool(routes))
        page.clock.run_for(21000)
        expect(page.get_by_role('alert')).to_contain_text('请求超时，请重试')
        with page.expect_request(lambda req:urlparse(req.url).path.endswith('/users')):
            initial_requests=len(f.user_requests)
            page.get_by_role('alert').get_by_role('button',name='重试',exact=True).click()
        wait_routes(page,f,lambda routes:len(routes)>initial_requests)
        f.user_requests[-1].fulfill(json=f.users('retry recovered'))
        expect(page.get_by_text('retry recovered',exact=True)).to_be_visible()
        close_page(page,f);browser.close();print(engine,'deadline and explicit retry passed',flush=True)
