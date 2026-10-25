# UI-only readiness and shortcut coverage. Native Snell auth/billing use Go tests.
import ast
from pathlib import Path
source=Path(__file__).with_name('operations-ui-smoke.py').read_text();tree=ast.parse(source)
exec(compile(ast.Module(body=[n for n in tree.body if not isinstance(n,ast.With)],type_ignores=[]),str(Path(__file__).with_name('operations-ui-smoke.py')),'exec'))
node['cores']=[dict(id='c1',nodeId='n1',type='singbox',version='1.14.4',status='running',installedVersions=[])]
node['defaultCore']='singbox'
caps[:]=[dict(core='singbox',version='1.14.4',combos=[dict(protocol='snell',transport='tcp',security='none'),dict(protocol='vless',transport='tcp',security='none')],features=[],hotUsers=False)]
class SnellEntryFixture(Fixture):
 def __init__(self):super().__init__();self.ready=False
 def route(self,route):
  path=urlparse(route.request.url).path.removeprefix('/api/v1')
  if path.endswith('/snell-readiness'):result=dict(ready=self.ready,minVersion='1.14.0',installedVersion='1.14.4',reasons=[] if self.ready else ['先升级节点 Agent，并等待重新连接上报 Snell 用户授权能力。'])
  elif path=='/users':result=dict(items=[dict(id='u1',username='Alice')])
  elif path=='/inbounds' and route.request.method=='POST':
   self.writes.append(('POST',path,route.request.post_data_json,route.request.headers));result=dict(id='created')
  else:return super().route(route)
  route.fulfill(status=200,content_type='application/json',body=json.dumps(result))
with sync_playwright() as p:
 browser=p.chromium.launch(executable_path=CHROMIUM,headless=True,args=['--no-sandbox','--disable-dev-shm-usage'])
 for width in [390,1440]:
  f=SnellEntryFixture();page=browser.new_page(viewport=dict(width=width,height=1000),locale='zh-CN');page.set_default_timeout(10000)
  page.add_init_script("localStorage.setItem('polaris.token','fixture');localStorage.setItem('polaris.lang','zh');localStorage.setItem('polaris.refresh','0')")
  page.route('**/api/v1/**',f.route);errors=[];page.on('pageerror',lambda e:errors.append(str(e)));page.goto(BASE_URL+'/inbounds')
  page.get_by_role('button',name='Snell 入站',exact=True).click()
  expect(page.get_by_role('button',name='配置 Snell 入站',exact=True)).to_be_disabled()
  expect(page.get_by_text('先升级节点 Agent，并等待重新连接上报 Snell 用户授权能力。',exact=True)).to_be_visible()
  f.ready=True;page.get_by_role('button',name='重新检查',exact=True).click()
  expect(page.get_by_role('button',name='配置 Snell 入站',exact=True)).to_be_enabled()
  page.get_by_role('button',name='配置 Snell 入站',exact=True).click()
  expect(page.get_by_label('Snell 授权模式',exact=True)).to_have_value('psk')
  expect(page.get_by_role('button',name='保存并下发',exact=True)).to_be_disabled()
  page.get_by_label('Snell 专属用户',exact=True).select_option('u1')
  page.get_by_label('端口',exact=True).fill('34443')
  page.get_by_role('button',name='保存并下发',exact=True).click()
  expect(page.get_by_role('button',name='+ 新建入站',exact=True)).to_be_visible()
  saved=f.last('/inbounds')[2]
  assert saved['core']=='singbox' and saved['protocol']==dict(type='snell',snellAuth='psk',snellVersion=5,snellUserId='u1'),saved['protocol']
  no_overflow(page);assert not errors,errors;page.close()
  print(width,'visible Snell shortcut, actionable readiness, dedicated owner and create payload passed',flush=True)
 browser.close()
