# Mocked frontend regression; real authentication/accounting use Go live tests.
import ast
from pathlib import Path
source=Path(__file__).with_name('operations-ui-smoke.py').read_text()
tree=ast.parse(source)
exec(compile(ast.Module(body=[n for n in tree.body if not isinstance(n,ast.With)],type_ignores=[]),str(Path(__file__).with_name('operations-ui-smoke.py')),'exec'))
src=dict(id='s1',name='Provider',kind='manual',hasContent=True,hasUrl=False,urlHost='',userAgent='',group='',enabled=True,refreshMin=0,nodeCount=1,userInfo={},revision=2,remark='',sortOrder=0)
resource=dict(id='stable-node-1',sourceId='s1',name='Hong Kong upstream',protocol='trojan',server='upstream.example.com',port=443,network='tcp',security='tls',missing=False,available=True,supported=True,source='Provider',references=[])
removed=dict(resource,id='stable-node-2',name='Removed upstream',missing=True,available=False,supported=False,reason='已移除')
class SourceFixture(Fixture):
 def route(self,route):
  req=route.request;path=urlparse(req.url).path.removeprefix('/api/v1')
  result=None;status=200
  if path=='/sub-sources': result=[src]
  elif path=='/sub-meta': result=dict(formats=['links'],regions=[],schemes=[])
  elif path=='/sub-profiles':result=[]
  elif path=='/sub-sources/s1/nodes': result=[resource,removed]
  elif path=='/sub-source-nodes': result=[resource,removed]
  elif path=='/sub-sources/s1/refreshes':result=[dict(id='refresh',revision=2,createdAt=now,unchanged=0,changes=[dict(id=resource['id'],name=resource['name'],kind='updated',fields=['credentials']),dict(id=removed['id'],name=removed['name'],kind='removed')])]
  elif path=='/sub-sources/s1' and req.method=='PUT':result=dict(source=src);self.writes.append((req.method,path,req.post_data_json,req.headers))
  elif path.endswith('/references'):result=dict(id='out-new');status=201;self.writes.append((req.method,path,req.post_data_json,req.headers))
  if result is not None:route.fulfill(status=status,content_type='application/json',body=json.dumps(result));return
  return super().route(route)

with sync_playwright() as p:
 browser=p.chromium.launch(executable_path=CHROMIUM,headless=True,args=['--no-sandbox','--disable-dev-shm-usage'])
 for width in [390,1440]:
  f=SourceFixture();page=browser.new_page(viewport=dict(width=width,height=1000),locale='zh-CN',timezone_id='Asia/Shanghai');page.set_default_timeout(10000)
  page.add_init_script("localStorage.setItem('polaris.token','fixture');localStorage.setItem('polaris.lang','zh');localStorage.setItem('polaris.refresh','0')")
  page.route('**/api/v1/**',f.route);errors=[];page.on('pageerror',lambda e:errors.append(str(e)))
  page.goto(BASE_URL+'/convert');page.get_by_role('tab',name='订阅源',exact=True).click()
  page.get_by_role('button',name='编辑',exact=True).click()
  expect(page.get_by_label('节点内容',exact=True)).to_have_value('')
  expect(page.get_by_role('button',name='保存并解析',exact=True)).to_be_enabled()
  page.get_by_role('button',name='保存并解析',exact=True).click()
  expect(page.get_by_role('button',name='编辑',exact=True)).to_be_visible()
  saved=f.last('/sub-sources/s1')[2];assert saved['content']=='' and saved['name']=='Provider',saved
  page.get_by_role('table').get_by_role('button',name='节点',exact=True).click()
  expect(page.get_by_text('stable-node-1',exact=True)).to_be_visible()
  expect(page.get_by_role('cell',name='Removed upstream stable-node-2 trojan',exact=True)).to_be_visible()
  page.get_by_role('switch',name='显示已移除节点',exact=True).click()
  expect(page.get_by_role('cell',name='Removed upstream stable-node-2 trojan',exact=True)).to_have_count(0)
  page.get_by_role('tab',name='刷新差异',exact=True).click()
  expect(page.locator('li').filter(has_text='Hong Kong upstream')).to_be_visible()
  expect(page.get_by_text('stable-node-1',exact=True)).to_be_visible()
  page.get_by_role('tab',name='节点与引用',exact=True).click()
  page.get_by_role('button',name='引用',exact=True).click()
  page.get_by_label('服务器',exact=True).select_option('n1')
  page.get_by_label('中转入站（可选）',exact=True).select_option('i1')
  page.get_by_role('button',name='创建引用',exact=True).click()
  expect(page.get_by_role('button',name='引用',exact=True)).to_be_visible()
  payload=f.last('/sub-source-nodes/stable-node-1/references')[2]
  assert payload==dict(nodeId='n1',tag='source-stable-n',inboundId='i1'),payload
  no_overflow(page);assert not errors,errors
  page.screenshot(path=str(OUT/f'source-resources-{width}.png'));page.close()
  print(width,'secret-preserving edit, stable IDs, removed filter, refresh differences and relay binding OK',flush=True)
 browser.close()
