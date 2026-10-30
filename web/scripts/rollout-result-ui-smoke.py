"""Real rollout panel: terminal results dismiss locally; ongoing work stays visible."""
import copy,json,os,shutil
from datetime import datetime,timedelta,timezone
from pathlib import Path
from urllib.parse import urlparse
from playwright.sync_api import sync_playwright,expect

BASE=os.environ.get('POLARIS_WEB_TEST_URL','http://127.0.0.1:5187').rstrip('/')
OUT=Path(os.environ.get('POLARIS_ROLLOUT_ARTIFACTS','/tmp/polaris-rollouts-ui'));OUT.mkdir(exist_ok=True,parents=True)
now=datetime.now(timezone.utc)
def result(id='failed',state='failed'):
    at=now.isoformat()
    return dict(id=id,kind='agent',version='v0.1.186',nodeIds=['ORACLE-SG'],canary=1,batchSize=4,soakSeconds=120,state=state,cursor=1,batch=[],results={'ORACLE-SG':dict(state='failed' if state=='failed' else 'ok',error='another agent upgrade is pending',at=at)},lastError='another agent upgrade is pending' if state=='failed' else '',createdAt=at,updatedAt=at)

class Fixture:
    def __init__(self):
        self.items=[result(),result('older')];self.writes=[];self.calls=0
    def route(self,route):
        req=route.request;path=urlparse(req.url).path.removeprefix('/api/v1')
        if req.method not in ['GET','HEAD']:self.writes.append((req.method,path))
        value=[]
        if path=='/auth/me':value=dict(username='fixture',role='owner')
        elif path=='/info':value=dict(title='Polaris',version='v0.1.186',edition='community')
        elif path=='/license':value=dict(edition='community',state=dict(plan='community',features=[],warnings=[],limits={}),usage={})
        elif path=='/settings':value={}
        elif path=='/rollouts':self.calls+=1;value=dict(items=copy.deepcopy(self.items))
        route.fulfill(json=value)

with sync_playwright() as p:
    for engine in ['chromium','webkit']:
        browser=p.chromium.launch(executable_path=os.environ.get('POLARIS_CHROMIUM_BIN') or shutil.which('chromium'),args=['--no-sandbox','--disable-dev-shm-usage']) if engine=='chromium' else p.webkit.launch()
        for width in [390,1440]:
            for lang in ['zh','en']:
                f=Fixture();page=browser.new_page(viewport=dict(width=width,height=1000),locale='zh-CN' if lang=='zh' else 'en-US')
                page.set_default_timeout(8000);page.clock.install()
                page.add_init_script("localStorage.setItem('polaris.token','fixture');localStorage.setItem('polaris.refresh','0');localStorage.setItem('polaris.lang',%s)"%json.dumps(lang))
                page.route('**/api/v1/**',f.route)
                errors=[];page.on('pageerror',lambda e:errors.append(str(e)))
                label='升级任务' if lang=='zh' else 'Upgrade task'
                view='查看详情' if lang=='zh' else 'View details'
                dismiss='关闭升级结果' if lang=='zh' else 'Dismiss upgrade result'
                panel=page.get_by_role('region',name=label)
                page.goto(BASE+'/nodes');expect(panel).to_be_visible()
                expect(panel).to_contain_text('Agent → v0.1.186');expect(panel).not_to_contain_text('vv0.1.186')
                expect(panel.get_by_text('another agent upgrade is pending',exact=True)).not_to_be_visible()
                details=panel.get_by_role('button',name=view,exact=True);expect(details).to_have_attribute('aria-expanded','false')
                details.click();expect(panel.get_by_text('another agent upgrade is pending',exact=True)).to_be_visible()
                page.screenshot(path=str(OUT/f'{engine}-{width}-{lang}-expanded.png'))
                panel.get_by_role('button',name='收起' if lang=='zh' else 'Collapse',exact=True).click()
                expect(panel.get_by_text('another agent upgrade is pending',exact=True)).not_to_be_visible()
                panel.get_by_role('button',name=dismiss,exact=True).click();expect(panel).to_have_count(0)
                calls=f.calls;page.clock.run_for(60010)
                page.wait_for_timeout(50);assert f.calls>calls
                expect(panel).to_have_count(0) # polling must not resurrect a closed result or show an older one
                page.reload();expect(page.get_by_role('heading',level=1)).to_be_visible();expect(panel).to_have_count(0)
                page.goto(BASE+'/cores');expect(page.get_by_role('heading',level=1)).to_be_visible();expect(panel).to_have_count(0)
                # Same ID, updated result is new information and becomes visible again.
                f.items[0]['updatedAt']=(now+timedelta(seconds=1)).isoformat()
                page.clock.run_for(60010);expect(panel).to_be_visible()
                panel.get_by_role('button',name=dismiss,exact=True).click();expect(panel).to_have_count(0)
                # New jobs are not suppressed by a previously dismissed job.
                f.items=[result('new-done','done')];page.clock.run_for(60010);expect(panel).to_be_visible()
                panel.get_by_role('button',name=dismiss,exact=True).click();expect(panel).to_have_count(0)
                # Active work has priority even if a newer historical record exists.
                f.items=[result('newer-failed'),result('active','running')]
                page.clock.run_for(60010);expect(panel.get_by_role('button',name='暂停' if lang=='zh' else 'Pause',exact=True)).to_be_visible()
                expect(panel.get_by_role('button',name=dismiss,exact=True)).to_have_count(0)
                expect(panel.get_by_role('progressbar')).to_be_visible()
                f.items[1]['state']='paused';page.clock.run_for(5010)
                expect(panel.get_by_role('button',name='继续' if lang=='zh' else 'Resume',exact=True)).to_be_visible()
                f.items[1]['state']='rolling_back';page.clock.run_for(5010)
                expect(panel.get_by_role('button',name=dismiss,exact=True)).to_have_count(0)
                expect(panel.get_by_role('progressbar')).to_be_visible()
                # Canceled results can also be dismissed; closing is never a server mutation.
                f.items=[result('canceled','canceled')];page.clock.run_for(5010)
                expect(panel.get_by_role('button',name=dismiss,exact=True)).to_be_visible()
                assert page.evaluate('document.documentElement.scrollWidth<=innerWidth+1')
                page.screenshot(path=str(OUT/f'{engine}-{width}-{lang}-compact.png'))
                panel.get_by_role('button',name=dismiss,exact=True).click();expect(panel).to_have_count(0)
                assert not f.writes,f.writes
                assert not errors,errors
                page.close();print(engine,width,lang,'terminal details, persistent dismissal, updated/new results, active priority OK',flush=True)
        # Storage may be unavailable. Closing must still work in the mounted page.
        f=Fixture();page=browser.new_page();page.route('**/api/v1/**',f.route)
        page.add_init_script("localStorage.setItem('polaris.token','fixture'); localStorage.setItem('polaris.lang','zh'); Storage.prototype.setItem=function(){throw new Error('storage unavailable')}")
        page.goto(BASE+'/nodes');panel=page.get_by_role('region',name='升级任务')
        panel.get_by_role('button',name='关闭升级结果').click();expect(panel).to_have_count(0)
        page.close();browser.close()
