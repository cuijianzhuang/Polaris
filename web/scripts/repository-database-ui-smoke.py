# Preset repository install controls, with mock API responses.
import ast
from pathlib import Path
source=Path(__file__).with_name('city-settings-ui-smoke.py').read_text();tree=ast.parse(source)
exec(compile(ast.Module(body=[n for n in tree.body if not isinstance(n,ast.With)],type_ignores=[]),str(Path(__file__).with_name('city-settings-ui-smoke.py')),'exec'))
class RepositoryFixture(CityFixture):
 def __init__(self):super().__init__();self.failure=False
 def route(self,route):
  req=route.request;path=urlparse(req.url).path.removeprefix('/api/v1')
  if path in ['/tools/route-asn/database/fetch','/tools/route-geo/database/fetch']:
   body=req.post_data_json;self.writes.append((req.method,path,body,req.headers))
   if self.failure:route.fulfill(status=502,content_type='application/json',body=json.dumps(dict(error='仓库下载失败，请稍后重试或切换来源')));return
   geo='/route-geo/' in path;value=self.geo if geo else self.asn
   value['policy'].update(sha256=('b' if geo else 'a')*64,revision=value['policy']['revision']+1)
   value.update(databaseState='ready',database=dict(databaseType='GeoIP2-City' if geo else 'GeoLite2-ASN',buildEpoch=1761300000,ipVersion=6),effectiveEnabled=value['policy']['enabled'])
   route.fulfill(status=200,content_type='application/json',body=json.dumps(value))
  else:return super().route(route)
with sync_playwright() as p:
 browser=p.chromium.launch(executable_path=CHROMIUM,headless=True,args=['--no-sandbox','--disable-dev-shm-usage'])
 for width in [390,1440]:
  for lang in ['zh','en']:
   f=RepositoryFixture();page=browser.new_page(viewport=dict(width=width,height=1000),locale='zh-CN' if lang=='zh' else 'en-US');page.set_default_timeout(10000)
   page.add_init_script("localStorage.setItem('polaris.token','fixture');localStorage.setItem('polaris.lang',%s);localStorage.setItem('polaris.refresh','0')"%json.dumps(lang))
   errors=[];page.on('pageerror',lambda e:errors.append(str(e)));page.route('**/api/v1/**',f.route);page.goto(BASE_URL+'/settings#network')
   for kind,source in [('asn','loyalsoldier-cdn'),('geo','free-geoip')]:
    if kind=='geo':page.get_by_role('button',name='地理归属' if lang=='zh' else 'Geographic attribution',exact=True).click()
    switch=page.get_by_role('switch');expect(switch).to_be_disabled()
    select=page.locator('#'+kind+'-repository');expect(select).to_be_enabled();select.select_option(source)
    button=page.get_by_role('button',name='从仓库获取并安装' if lang=='zh' else 'Fetch and install from repository',exact=True);button.click()
    expect(switch).to_be_enabled();expect(switch).to_have_attribute('aria-checked','false')
    saved=f.last('/tools/route-'+kind+'/database/fetch');assert saved[2]==dict(source=source,expectedRevision=0),saved;assert saved[3]['authorization']=='Bearer fixture'
    expect(page.locator('#'+kind+'-database-file')).to_be_visible()
    switch.click();expect(switch).to_have_attribute('aria-checked','true')
    f.failure=True;button.click();expect(page.get_by_text('仓库下载失败，请稍后重试或切换来源' if lang=='zh' else 'Repository download failed; try again later or choose another source',exact=True).last).to_be_visible()
    expect(button).to_be_enabled();expect(switch).to_have_attribute('aria-checked','true');f.failure=False
    no_overflow(page);page.screenshot(path=str(OUT/f'repository-{kind}-{lang}-{width}.png'),full_page=True,animations='disabled',style='.toasts { visibility: hidden; }')
   page.get_by_role('link',name='查看回程检测' if lang=='zh' else 'View route traces',exact=True).click()
   expect(page.get_by_role('button',name='立即检测回程' if lang=='zh' else 'Trace route now',exact=True)).to_be_visible()
   assert 'network=route' in page.url
   assert not any(w[1]=='/nodes/n1/route-trace' for w in f.writes), 'navigation started a live trace'
   page.get_by_role('button',name='详情' if lang=='zh' else 'Details',exact=True).first.click()
   expect(page.get_by_role('dialog').get_by_text('AS13335 · Cloudflare',exact=True)).to_be_visible()
   assert not errors,errors;page.close();print(width,lang,'ASN/City preset fetch, CAS payload, auth, explicit activation, preserved state on failure and manual upload passed',flush=True)
 browser.close()
