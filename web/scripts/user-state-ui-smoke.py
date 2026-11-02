"""Single-user switches never use the bulk selection and wait for confirmation."""
import json,os,shutil
from pathlib import Path
from urllib.parse import urlparse
from playwright.sync_api import sync_playwright,expect
scope={'__file__':str(Path(__file__).with_name('user-online-ui-smoke.py'))}
exec(Path(scope['__file__']).read_text().rsplit('\nwith sync_playwright() as p:',1)[0],scope)
Base=scope['Fixture'];BASE=os.environ.get('POLARIS_WEB_TEST_URL','http://127.0.0.1:5187').rstrip('/')
OUT=Path(os.environ.get('POLARIS_USER_STATE_ARTIFACTS','/tmp/polaris-user-state'));OUT.mkdir(parents=True,exist_ok=True)
class Fixture(Base):
 def __init__(self):super().__init__();self.pending=None;self.writes=[]
 def route(self,route):
  req=route.request;path=urlparse(req.url).path.removeprefix('/api/v1')
  if req.method=='PUT' and path.endswith('/state'):
   assert self.pending is None
   assert path=='/users/known/state'
   assert set(req.post_data_json)=={'enabled'}
   self.writes.append((path,req.post_data_json));self.pending=route
   return
  if req.method!='GET' and path.startswith('/users'):raise AssertionError('single switch must not use bulk or full editor: '+path)
  return super().route(route)
 def reply(self,fail=False):
  route=self.pending;self.pending=None
  if fail:route.fulfill(status=503,json={'error':'fixture state failed'});return
  self.users['known']['status']='active' if route.request.post_data_json['enabled'] else 'disabled'
  route.fulfill(json=self.users['known'])
with sync_playwright() as p:
 for engine in ['chromium','webkit']:
  opts=dict(executable_path=os.environ.get('POLARIS_CHROMIUM_BIN') or shutil.which('chromium'),args=['--no-sandbox','--disable-dev-shm-usage']) if engine=='chromium' else {}
  browser=getattr(p,engine).launch(**opts)
  for width in [390,1440]:
   for lang in ['zh','en']:
    zh=lang=='zh';f=Fixture();page=browser.new_page(viewport=dict(width=width,height=1000));page.set_default_timeout(10000)
    page.add_init_script("localStorage.setItem('polaris.token','fixture');localStorage.setItem('polaris.refresh','0');localStorage.setItem('polaris.lang',%s)"%json.dumps(lang))
    errors=[];page.on('pageerror',lambda e,errors=errors:errors.append(str(e)));page.route('**/api/v1/**',f.route)
    page.goto(BASE+'/users');rows=page.locator('[data-user-id]');expect(rows).to_have_count(8)
    row=page.locator('[data-user-id="known"]');other=page.locator('[data-user-id="unlimited"]')
    switch=row.get_by_role('switch',name='启用用户 known' if zh else 'Enable user known',exact=True)
    other_switch=other.get_by_role('switch')
    other.get_by_role('checkbox').check() # A bulk selection must have no effect on this row's switch.
    expect(switch).to_have_attribute('aria-checked','true');switch.click();expect(switch).to_be_disabled()
    expect(switch).to_have_attribute('aria-checked','true');expect(other_switch).to_be_enabled();expect(other_switch).to_have_attribute('aria-checked','true')
    expect(page.get_by_role('dialog')).to_have_count(0)
    assert f.writes==[('/users/known/state',{'enabled':False})]
    f.reply();expect(switch).to_be_enabled();expect(switch).to_have_attribute('aria-checked','false');expect(other_switch).to_have_attribute('aria-checked','true')
    switch.click();expect(switch).to_be_disabled();f.reply(fail=True);expect(switch).to_be_enabled();expect(switch).to_have_attribute('aria-checked','false')
    expect(page.locator('.toast.err')).to_contain_text('fixture state failed')
    switch.click();expect(switch).to_be_disabled();f.reply();expect(switch).to_be_enabled();expect(switch).to_have_attribute('aria-checked','true')
    assert all(f.users[n]['status']=='active' for n in f.users)
    assert len(f.writes)==3 and page.evaluate('document.documentElement.scrollWidth<=innerWidth+1')
    page.screenshot(animations='disabled',path=str(OUT/f'{engine}-{width}-{lang}-user-state.png'))
    assert not errors,(engine,width,lang,errors)
    page.close();print(engine,width,lang,'isolated switch, pending confirmation, failure retention and recovery OK',flush=True)
  browser.close()
