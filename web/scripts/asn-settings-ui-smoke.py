# Mocked API coverage for ASN settings; offline lookup behavior is tested in Go.
import ast
from pathlib import Path
source=Path(__file__).with_name('operations-ui-smoke.py').read_text(); tree=ast.parse(source)
exec(compile(ast.Module(body=[n for n in tree.body if not isinstance(n,ast.With)],type_ignores=[]),str(Path(__file__).with_name('operations-ui-smoke.py')),'exec'))

class ASNFixture(Fixture):
 def route(self,route):
  req=route.request; path=urlparse(req.url).path.removeprefix('/api/v1')
  if path=='/tools/route-asn/lookup':
   body=req.post_data_json; self.writes.append((req.method,path,body,req.headers))
   ip=body['ip']
   match=dict(ip=ip,state='private' if ip=='10.0.0.1' else 'not_found' if ip=='203.0.113.1' else 'matched',asn=13335,organization='Cloudflare',network='1.1.1.0/24')
   route.fulfill(status=200,content_type='application/json',body=json.dumps(dict(match=match,databaseSHA256=self.asn['policy']['sha256'],effectiveEnabled=self.asn['effectiveEnabled'])))
  else: super().route(route)

with sync_playwright() as p:
 browser=p.chromium.launch(executable_path=CHROMIUM,headless=True,args=['--no-sandbox','--disable-dev-shm-usage'])
 for width in [390,1440]:
  for lang in ['zh','en']:
   f=ASNFixture(); page=browser.new_page(viewport=dict(width=width,height=1000),locale='zh-CN' if lang=='zh' else 'en-US');page.set_default_timeout(10000)
   page.add_init_script("localStorage.setItem('polaris.token','fixture');localStorage.setItem('polaris.lang',%s);localStorage.setItem('polaris.refresh','0')"%json.dumps(lang))
   page.route('**/api/v1/**',f.route);errors=[];page.on('pageerror',lambda e:errors.append(str(e)))
   page.goto(BASE_URL+'/settings#network')
   switch=page.get_by_role('switch');expect(switch).to_be_disabled()
   if width==390:
    box=page.locator('.snav-item.on').bounding_box();assert box['x']>=0 and box['x']+box['width']<=width,box
   upload=page.get_by_role('button',name='上传数据库' if lang=='zh' else 'Upload database',exact=True)
   lookup=page.get_by_role('button',name='查询归属' if lang=='zh' else 'Look up ASN',exact=True)
   expect(lookup).to_be_disabled()
   expect(page.get_by_role('link',name='GeoLite2-ASN.mmdb · GitHub ↗',exact=True)).to_have_attribute('href','https://raw.githubusercontent.com/Loyalsoldier/geoip/release/GeoLite2-ASN.mmdb')
   page.locator('#asn-database-file').set_input_files(dict(name='wrong.txt',mimeType='text/plain',buffer=b'wrong'))
   expect(upload).to_be_disabled(); expect(page.get_by_role('alert')).to_be_visible()
   page.locator('#asn-database-file').set_input_files(dict(name='empty.mmdb',mimeType='application/octet-stream',buffer=b''))
   expect(upload).to_be_disabled()
   page.locator('#asn-database-file').set_input_files(dict(name='asn.mmdb',mimeType='application/octet-stream',buffer=b'MMDB fixture bytes'))
   expect(upload).to_be_enabled();upload.click();expect(switch).to_be_enabled();expect(switch).to_have_attribute('aria-checked','false')
   assert f.last('/tools/route-asn/database')[3]['authorization']=='Bearer fixture'
   page.locator('#asn-lookup-ip').fill('1.1.1.1');lookup.click()
   expect(page.get_by_text('AS13335 · Cloudflare',exact=True)).to_be_visible()
   assert f.last('/tools/route-asn/lookup')[2]==dict(ip='1.1.1.1')
   expect(switch).to_have_attribute('aria-checked','false')
   page.locator('#asn-lookup-ip').fill('10.0.0.1');lookup.click()
   expect(page.get_by_text('私网或特殊地址，不查询 ASN 归属。' if lang=='zh' else 'Private or special-purpose address; ASN attribution is not queried.',exact=True)).to_be_visible()
   switch.click();expect(switch).to_have_attribute('aria-checked','true')
   # Missing local files must be visibly different from an intentionally off policy.
   f.asn['databaseState']='local_database_missing'; f.asn['database']=None; f.asn['effectiveEnabled']=False
   page.get_by_role('button',name='刷新' if lang=='zh' else 'Refresh',exact=True).click()
   expect(page.get_by_text('已开启 · 当前实例未生效' if lang=='zh' else 'Enabled · unavailable on this instance',exact=True)).to_be_visible()
   expect(lookup).to_be_disabled();expect(switch).to_be_enabled()
   switch.click();expect(switch).to_have_attribute('aria-checked','false')
   no_overflow(page)
   # Capture ready state in dark mode for the same layout reported by the user.
   f.asn['databaseState']='ready';f.asn['database']=dict(databaseType='GeoLite2-ASN',buildEpoch=1761300000,ipVersion=6)
   page.get_by_role('button',name='刷新' if lang=='zh' else 'Refresh',exact=True).click();expect(switch).to_be_enabled()
   page.evaluate("document.documentElement.dataset.mode='dark'")
   no_overflow(page);page.screenshot(path=str(OUT/f'asn-settings-{lang}-{width}.png'),full_page=True,animations='disabled',style='.toasts { visibility: hidden; }')
   assert not errors,errors;page.close();print(width,lang,'upload validation, explicit activation, local preview, missing-instance recovery, no overflow passed',flush=True)
 browser.close()
