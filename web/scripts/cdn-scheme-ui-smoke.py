# HTTP/HTTPS CDN config and one-click inbound regression; API fixtures only.
import copy, json, os, shutil
from pathlib import Path
from urllib.parse import urlparse
from playwright.sync_api import sync_playwright, expect

scope={'__file__':str(Path(__file__).with_name('cdn-domains-ui-smoke.py'))}
exec(Path(__file__).with_name('cdn-domains-ui-smoke.py').read_text().rsplit('\nwith sync_playwright() as p:',1)[0],scope)
BaseFixture=scope['Fixture']
BASE=os.environ.get('POLARIS_WEB_TEST_URL','http://127.0.0.1:5187').rstrip('/')
OUT=Path(os.environ.get('POLARIS_CDN_ARTIFACTS','/tmp/polaris-cdn-ui'));OUT.mkdir(parents=True,exist_ok=True)

class Fixture(BaseFixture):
 def __init__(self):
  super().__init__()
  self.profile.update(scheme='https',ports=[443,2053,2083,2087,2096,8443],caddyFronting=True)
  self.created=None
 def route(self,route):
  req=route.request;path=urlparse(req.url).path.removeprefix('/api/v1')
  if path=='/capabilities':
   combos=[dict(protocol='vless',transport=tr,security=sec) for tr in ['ws','httpupgrade','xhttp'] for sec in ['none','tls']]+[dict(protocol='vless',transport='tcp',security='reality'),dict(protocol='vless',transport='grpc',security='tls')]
   return route.fulfill(json=[dict(core='xray',version='26.5.9',combos=combos,features=[],hotUsers=True)])
  if path=='/cdn-profiles' and req.method=='GET':
   return route.fulfill(json=[self.profile])
  if path=='/cdn-profiles' and req.method=='POST':
   self.profile.update(req.post_data_json)
   self.writes.append((req.method,path,req.post_data_json,req.headers))
   return route.fulfill(status=201,json=self.profile)
  if path=='/inbounds' and req.method=='POST':
   self.created=req.post_data_json
   self.writes.append((req.method,path,req.post_data_json,req.headers))
   return route.fulfill(status=201,json=dict(self.created,id='new'))
  return super().route(route)

with sync_playwright() as p:
 for engine in ['chromium','webkit']:
  opts=dict(headless=True)
  if engine=='chromium':opts.update(executable_path=os.environ.get('POLARIS_CHROMIUM_BIN') or shutil.which('chromium'),args=['--no-sandbox','--disable-dev-shm-usage'])
  browser=getattr(p,engine).launch(**opts)
  for width in [390,1440]:
   for lang in ['zh','en']:
    f=Fixture();page=browser.new_page(viewport=dict(width=width,height=1000),locale='zh-CN' if lang=='zh' else 'en-US')
    page.set_default_timeout(10000)
    page.add_init_script("localStorage.setItem('polaris.token','fixture');localStorage.setItem('polaris.refresh','0');localStorage.setItem('polaris.lang',%s)"%json.dumps(lang))
    errors=[];page.on('pageerror',lambda e:errors.append(str(e)));page.route('**/api/v1/**',f.route)
    page.goto(BASE+'/chains')
    page.get_by_role('tab',name='CDN 配置档' if lang=='zh' else 'CDN profile',exact=True).click()
    page.get_by_role('button',name='编辑' if lang=='zh' else 'Edit',exact=True).last.click()
    expect(page.locator('#cdn-scheme')).to_have_value('https')
    expect(page.get_by_role('switch',name='Caddy',exact=True)).to_have_attribute('aria-checked','true')
    page.locator('#cdn-scheme').select_option('http')
    expect(page.locator('#cdn-ports')).to_have_value('80, 8080, 8880, 2052, 2082, 2086, 2095')
    expect(page.locator('#cdn-origin-tls')).to_be_disabled()
    expect(page.get_by_role('switch',name='Caddy',exact=True)).to_be_disabled()
    expect(page.get_by_role('switch',name='Caddy',exact=True)).to_have_attribute('aria-checked','false')
    assert page.evaluate('document.documentElement.scrollWidth <= innerWidth + 1')
    page.screenshot(path=str(OUT/f'{engine}-{width}-{lang}-http-form.png'))
    # Switching back resets HTTP ports and re-enables the independent SSL mode.
    page.locator('#cdn-scheme').select_option('https')
    expect(page.locator('#cdn-ports')).to_have_value('443, 2053, 2083, 2087, 2096, 8443')
    expect(page.locator('#cdn-origin-tls')).to_be_enabled()
    page.locator('#cdn-scheme').select_option('http')
    save='保存' if lang=='zh' else 'Save'
    page.get_by_role('dialog').get_by_role('button',name=save,exact=True).click()
    expect(page.get_by_role('dialog')).to_have_count(0)
    saved=f.last('/cdn-profiles/cf')[2]
    assert saved['scheme']=='http' and saved['ports']==[80,8080,8880,2052,2082,2086,2095] and not saved['caddyFronting'],saved
    expect(page.get_by_text('HTTP 回源（不使用此设置）' if lang=='zh' else 'HTTP origin (this setting is unused)',exact=True)).to_be_visible()
    page.get_by_text('添加测速点（一键安装 cf-ip-probe）' if lang=='zh' else 'Add a speed-test point (one-line cf-ip-probe install)',exact=True).click()
    page.get_by_label('测速点名称' if lang=='zh' else 'point name',exact=True).fill('http80')
    page.get_by_role('button',name='生成安装命令' if lang=='zh' else 'Generate install command',exact=True).click()
    expect(page.get_by_text('--args',exact=False).filter(has_text='-scheme http -port 80').first).to_be_visible()
    expect(page.get_by_text('--instance',exact=False).first).to_be_visible()
    # The one-click flow uses the profile's HTTP ports and disables origin TLS.
    page.goto(BASE+'/inbounds')
    page.get_by_role('button',name='+ 新建入站' if lang=='zh' else '+ New inbound',exact=True).click()
    page.get_by_role('button',name='一键配置 CDN' if lang=='zh' else 'Set up for CDN',exact=True).first.click()
    expect(page.locator('#ib-port')).to_have_value('80')
    page.get_by_role('button',name='gRPC',exact=True).click()
    expect(page.get_by_text('Cloudflare 的 gRPC 入口必须使用 HTTPS 和 443 端口。' if lang=='zh' else 'Cloudflare gRPC entries require HTTPS on port 443.',exact=True)).to_be_visible()
    page.get_by_role('button',name='一键配置 CDN' if lang=='zh' else 'Set up for CDN',exact=True).first.click()
    expect(page.get_by_role('button',name='无' if lang=='zh' else 'None',exact=True)).to_have_attribute('class',__import__('re').compile(r'\bon\b'))
    page.locator('#ib-tag').fill('http-cdn-fixture')
    page.get_by_role('dialog').get_by_role('button',name='保存并下发' if lang=='zh' else 'Save and push',exact=True).click()
    expect(page.get_by_role('dialog')).to_have_count(0)
    assert f.created['port']==80 and f.created['security']['type']=='none' and f.created['transport']['type']=='ws' and f.created['cdnOnly'] and f.created['cdnProfileId']=='cf',f.created
    assert not errors,errors
    page.close();print(engine,width,lang,'HTTP/HTTPS ports, SSL separation, Caddy guard, HTTP inbound OK',flush=True)
  browser.close()
