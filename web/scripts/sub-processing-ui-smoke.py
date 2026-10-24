# Reuses the operational UI fixture definitions without executing its scenarios.
# Requires Vite, Python Playwright and Chromium; see docs/design/sub-processing-groups.md.
import ast
from pathlib import Path
source=Path(__file__).with_name('operations-ui-smoke.py').read_text()
tree=ast.parse(source)
exec(compile(ast.Module(body=[n for n in tree.body if not isinstance(n,ast.With)],type_ignores=[]),str(Path(__file__).with_name('operations-ui-smoke.py')),'exec'))

class ProcessingFixture(Fixture):
 def __init__(self):
  super().__init__()
  self.groups=[dict(id='g1',name='Common rules',remark='',operators=[dict(type='dedup')],revision=1,createdAt=now,updatedAt=now)]
  self.profiles=[dict(id='p1',name='Combined',enabled=True,sourceIds=[],panelUserId='',operators=[],processingGroups=[dict(groupId='g1',revision=0)],useGlobalOps=False,templates={},converter='',backend='',backendConfig='',scheme='',skipCert=None,forceUdp=None,downloads=0,remark='',url='https://example.com/sub/p/secret')]
  self.references=[dict(id='p1',name='Combined',enabled=True,revision=0,followsLatest=True),dict(id='pinned',name='Pinned subscription',enabled=False,revision=1,followsLatest=False)]
  self.stale=False
 def route(self,route):
  req=route.request; path=urlparse(req.url).path.removeprefix('/api/v1')
  if path=='/license':
   result=dict(edition='pro',state=dict(plan='pro',features=['sub_convert'],warnings=[],limits={}),usage={})
  elif path=='/sub-meta': result=dict(formats=['links','singbox'],regions=[dict(Code='HK',Name='Hong Kong')])
  elif path=='/sub-profiles': result=self.profiles
  elif path=='/sub-profiles/p1': result=req.post_data_json;self.profiles=[dict(self.profiles[0],**result)]
  elif path=='/sub-processing-groups' and req.method=='GET': result=self.groups
  elif path=='/sub-processing-groups' and req.method=='POST':
   result=dict(req.post_data_json,id='g2',revision=1,createdAt=now,updatedAt=now);self.groups.append(result)
  elif path.endswith('/versions'):
   result=dict(versions=[dict(groupId='g1',revision=v,name='Common rules',createdAt=now) for v in [2,1]])
  elif path.endswith('/versions/1') or path.endswith('/versions/2'): result=dict(groupId='g1',revision=1,operators=[dict(type='rename',template='{name}')],createdAt=now)
  elif path.endswith('/preview'):
   result=dict(etag='"fixture-tag"',revision=1,action=req.post_data_json['action'],references=self.references,affected=1)
  elif path.startswith('/sub-processing-groups/') and req.method=='PUT':
   if self.stale:
    self.stale=False;route.fulfill(status=412,content_type='application/json',body=json.dumps(dict(error='处理组或引用已变化，请重新预览')));return
   result=dict(self.groups[0],**req.post_data_json);result['revision']=2;self.groups[0]=result
  else: return super().route(route)
  self.requests.append((req.method,path))
  if req.method in ['POST','PUT','DELETE']: self.writes.append((req.method,path,req.post_data_json,req.headers))
  route.fulfill(status=200,content_type='application/json',body=json.dumps(result))

def processing_page(browser,width):
 page=browser.new_page(viewport=dict(width=width,height=1000),locale='zh-CN')
 page.set_default_timeout(10000);f=ProcessingFixture();errors=[]
 page.add_init_script("localStorage.setItem('polaris.token','fixture');localStorage.setItem('polaris.lang','zh')")
 page.route('**/api/v1/**',f.route);page.on('pageerror',lambda e:errors.append(str(e)))
 page.goto(BASE_URL+'/convert')
 return page,f,errors

with sync_playwright() as p:
 browser=p.chromium.launch(executable_path=CHROMIUM,headless=True,args=['--no-sandbox','--disable-dev-shm-usage'])
 for width in [390,1440]:
  page,f,errors=processing_page(browser,width)
  page.get_by_role('tab',name='处理组',exact=True).click()
  page.get_by_role('button',name='+ 新建处理组',exact=True).click()
  page.get_by_label('处理组名称',exact=True).fill('Shared rules')
  page.get_by_role('button',name='+ 去重',exact=True).click();page.get_by_role('button',name='保存',exact=True).click()
  expect(page.get_by_role('dialog')).to_have_count(0)
  assert f.last('/sub-processing-groups')[2]['operators']==[dict(type='dedup')]
  page.get_by_role('button',name='编辑与影响预览',exact=True).first.click()
  page.get_by_label('处理组备注',exact=True).fill('updated')
  expect(page.get_by_role('button',name='保存',exact=True)).to_be_disabled()
  page.get_by_role('button',name='预览影响范围',exact=True).click();expect(page.get_by_role('button',name='保存',exact=True)).to_be_enabled()
  expect(page.get_by_text('Pinned subscription',exact=True)).to_be_visible()
  page.screenshot(path=str(OUT/f'processing-impact-{width}.png'))
  page.get_by_role('button',name='保存',exact=True).click();expect(page.get_by_role('dialog')).to_have_count(0)
  saved=f.last('/sub-processing-groups/g1');assert saved[2]['expectedRevision']==1 and saved[3]['if-match']=='"fixture-tag"'
  page.locator('section.card').filter(has=page.get_by_role('heading',name='Common rules',exact=True)).get_by_role('button',name='删除',exact=True).click()
  page.get_by_role('button',name='预览影响范围',exact=True).click();expect(page.get_by_role('button',name='确认删除',exact=True)).to_be_disabled()
  page.get_by_role('button',name='取消',exact=True).click();expect(page.get_by_role('dialog')).to_have_count(0)
  page.get_by_role('tab',name='订阅组',exact=True).click();page.get_by_role('button',name='编辑',exact=True).click()
  expect(page.get_by_label('处理组版本',exact=True)).to_have_value('0')
  page.get_by_label('处理组版本',exact=True).select_option('1')
  page.get_by_role('button',name='保存',exact=True).click();expect(page.get_by_role('dialog')).to_have_count(0)
  assert f.last('/sub-profiles/p1')[2]['processingGroups']==[dict(groupId='g1',revision=1)]
  no_overflow(page);assert not errors,errors;page.close()
  print(width,'create, preview, ETag, pinned/disabled delete protection and profile pin save OK',flush=True)
 page,f,errors=processing_page(browser,390);page.get_by_role('tab',name='处理组',exact=True).click();page.get_by_role('button',name='编辑与影响预览',exact=True).click()
 page.get_by_label('处理组备注',exact=True).fill('conflict');page.get_by_role('button',name='预览影响范围',exact=True).click();expect(page.get_by_role('button',name='保存',exact=True)).to_be_enabled();f.stale=True
 page.get_by_role('button',name='保存',exact=True).click();expect(page.get_by_text('处理组或引用已变化，请重新预览',exact=True)).to_be_visible();expect(page.get_by_role('button',name='保存',exact=True)).to_be_disabled()
 assert not errors,errors;page.close();browser.close();print('stale preview requires a new preview OK',flush=True)
