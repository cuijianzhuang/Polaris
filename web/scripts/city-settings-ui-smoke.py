# Browser interaction tests with mocked APIs; real MMDB/gzip behavior uses Go tests.
import ast
from pathlib import Path
source=Path(__file__).with_name('operations-ui-smoke.py').read_text();tree=ast.parse(source)
exec(compile(ast.Module(body=[n for n in tree.body if not isinstance(n,ast.With)],type_ignores=[]),str(Path(__file__).with_name('operations-ui-smoke.py')),'exec'))

class CityFixture(Fixture):
 def __init__(self):
  super().__init__();self.conflict=False
  self.geo=dict(policy=dict(enabled=False,sha256='',revision=0),databaseState='not_configured',database=None,effectiveEnabled=False)
  self.citytrace=copy.deepcopy(trace)
  self.citytrace['result']['geo']=dict(state='ready',databaseSha256='b'*64,databaseType='GeoIP2-City',buildEpoch=1)
  self.citytrace['result']['hops'][0]['geo']=[dict(ip='1.1.1.1',state='matched',countryCode='GB',country='英国',countryEn='United Kingdom',province='英格兰',provinceEn='England',city='伦敦',cityEn='London')]
 def route(self,route):
  req=route.request;path=urlparse(req.url).path.removeprefix('/api/v1');status=200
  if path.startswith('/tools/route-geo'):
   body=None
   if req.method!='GET':
    try:body=req.post_data_json
    except Exception:body=req.post_data_buffer
    self.writes.append((req.method,path,body,req.headers))
   if path=='/tools/route-geo/database':
    assert req.headers['content-type']=='application/octet-stream'
    if req.post_data_buffer==b'bad ASN fixture':status=400;result=dict(error='需要包含国家、省份、城市字段的 City 类型 MMDB 数据库')
    else:
     assert req.post_data_buffer==b'GZIP fixture bytes'
     self.geo=dict(policy=dict(enabled=False,sha256='b'*64,revision=1),databaseState='ready',database=dict(databaseType='GeoIP2-City',buildEpoch=1761300000,ipVersion=6),effectiveEnabled=False);result=self.geo
   elif path=='/tools/route-geo/lookup':
    ip=body['ip'];match=dict(ip=ip,state='matched',countryCode='GB',country='英国',countryEn='United Kingdom',province='英格兰',provinceEn='England',city='伦敦',cityEn='London',network='81.2.69.160/27')
    if ip=='2001:218::1':match=dict(ip=ip,state='matched',country='日本',countryEn='Japan')
    elif ip=='203.0.113.2':match=dict(ip=ip,state='matched',country='新加坡',countryEn='Singapore',city='新加坡',cityEn='Singapore')
    elif ip=='10.0.0.1':match=dict(ip=ip,state='private')
    elif ip=='203.0.113.1':match=dict(ip=ip,state='not_found')
    result=dict(databaseSHA256=self.geo['policy']['sha256'],match=match)
   else:
    if req.method=='PUT':
     if self.conflict:self.conflict=False;self.geo['policy']['revision']+=1;status=409;result=dict(error='地理归属配置已变化，请刷新后重试')
     else:self.geo['policy']['enabled']=body['enabled'];self.geo['policy']['revision']+=1;self.geo['effectiveEnabled']=body['enabled'] and self.geo['databaseState']=='ready'
    if status==200:result=self.geo
  elif path=='/nodes/n1/route-traces':result=dict(items=[self.citytrace])
  elif path=='/nodes/n1/route-traces/20':result=self.citytrace
  else:return super().route(route)
  route.fulfill(status=status,content_type='application/json',body=json.dumps(result))

with sync_playwright() as p:
 browser=p.chromium.launch(executable_path=CHROMIUM,headless=True,args=['--no-sandbox','--disable-dev-shm-usage'])
 for width in [390,1440]:
  for lang in ['zh','en']:
   f=CityFixture();page=browser.new_page(viewport=dict(width=width,height=1000),locale='zh-CN' if lang=='zh' else 'en-US');page.set_default_timeout(10000)
   page.add_init_script("localStorage.setItem('polaris.token','fixture');localStorage.setItem('polaris.lang',%s);localStorage.setItem('polaris.refresh','0');localStorage.setItem('polaris.mode','dark')"%json.dumps(lang))
   errors=[];page.on('pageerror',lambda e:errors.append(str(e)));page.route('**/api/v1/**',f.route)
   page.goto(BASE_URL+'/settings#network');page.get_by_role('button',name='地理归属' if lang=='zh' else 'Geographic attribution',exact=True).click()
   switch=page.get_by_role('switch');expect(switch).to_be_disabled()
   upload=page.get_by_role('button',name='上传城市库' if lang=='zh' else 'Upload city database',exact=True)
   page.locator('#geo-database-file').set_input_files(dict(name='bad.zip',mimeType='application/zip',buffer=b'bad'))
   expect(upload).to_be_disabled();expect(page.get_by_role('alert')).to_be_visible()
   page.locator('#geo-database-file').set_input_files(dict(name='asn.mmdb',mimeType='application/octet-stream',buffer=b'bad ASN fixture'))
   upload.click();expect(page.get_by_text('需要包含国家、省份、城市字段的 City 类型 MMDB 数据库' if lang=='zh' else 'A City MMDB database with compatible country, province and city fields is required',exact=True)).to_be_visible()
   expect(switch).to_be_disabled()
   page.locator('#geo-database-file').set_input_files(dict(name='GeoLite2-City.mmdb.gz',mimeType='application/gzip',buffer=b'GZIP fixture bytes'))
   upload.click();expect(switch).to_be_enabled();expect(switch).to_have_attribute('aria-checked','false')
   assert f.last('/tools/route-geo/database')[3]['authorization']=='Bearer fixture'
   lookup=page.get_by_role('button',name='查询地理归属' if lang=='zh' else 'Look up location',exact=True)
   for ip,result in [('81.2.69.160','英国 · 英格兰 · 伦敦' if lang=='zh' else 'United Kingdom · England · London'),('2001:218::1','日本' if lang=='zh' else 'Japan'),('203.0.113.2','新加坡' if lang=='zh' else 'Singapore'),('10.0.0.1','私网或特殊地址' if lang=='zh' else 'Private or special-purpose address'),('203.0.113.1','数据库未收录地理归属' if lang=='zh' else 'No location in this database')]:
    page.locator('#geo-lookup-ip').fill(ip);lookup.click();expect(page.get_by_text(result,exact=True)).to_be_visible();expect(switch).to_have_attribute('aria-checked','false')
   switch.click();expect(switch).to_have_attribute('aria-checked','true')
   f.conflict=True;switch.click();expect(page.get_by_text('地理归属配置已变化，请刷新后重试' if lang=='zh' else 'Geographic attribution settings changed; refresh and try again',exact=True)).to_be_visible();expect(switch).to_have_attribute('aria-checked','true')
   expect(switch).to_be_enabled();assert f.last('/tools/route-geo')[2]['expectedRevision']==2
   f.geo['databaseState']='local_database_missing';f.geo['effectiveEnabled']=False;f.geo['database']=None
   page.get_by_role('button',name='刷新' if lang=='zh' else 'Refresh',exact=True).click();expect(page.get_by_text('已开启 · 当前实例未生效' if lang=='zh' else 'Enabled · unavailable on this instance',exact=True)).to_be_visible()
   expect(switch).to_be_enabled();switch.click();expect(switch).to_have_attribute('aria-checked','false')
   expect(lookup).to_be_disabled();no_overflow(page)
   f.geo['databaseState']='ready';f.geo['database']=dict(databaseType='GeoIP2-City',buildEpoch=1761300000,ipVersion=6)
   page.get_by_role('button',name='刷新' if lang=='zh' else 'Refresh',exact=True).click();expect(lookup).to_be_enabled()
   page.locator('#geo-lookup-ip').fill('81.2.69.160');lookup.click();expect(page.get_by_text('英国 · 英格兰 · 伦敦' if lang=='zh' else 'United Kingdom · England · London',exact=True)).to_be_visible()
   page.screenshot(path=str(OUT/f'city-settings-{lang}-{width}.png'),full_page=True,animations='disabled',style='.toasts { visibility: hidden; }')
   page.goto(BASE_URL+'/nodes');page.get_by_role('button',name='回程与定时检测' if lang=='zh' else 'Route traces and schedule',exact=True).click()
   page.get_by_role('button',name='详情' if lang=='zh' else 'Details',exact=True).first.click()
   dialog=page.get_by_role('dialog');expect(dialog.get_by_role('columnheader',name='国家 / 省份 / 城市' if lang=='zh' else 'Country / Province / City',exact=True)).to_be_visible()
   expect(dialog.get_by_text('英国 · 英格兰 · 伦敦' if lang=='zh' else 'United Kingdom · England · London',exact=True)).to_be_visible()
   expect(dialog.get_by_text('GeoIP2-City',exact=False)).to_be_visible()
   no_overflow(page);page.screenshot(path=str(OUT/f'city-trace-{lang}-{width}.png'),full_page=True,animations='disabled',style='.toasts { visibility: hidden; }')
   assert not errors,errors;page.close();print(width,lang,'independent city tab, gzip upload, ASN rejection, bilingual lookup, conflicts, missing-instance state and saved route annotations passed',flush=True)
 browser.close()
