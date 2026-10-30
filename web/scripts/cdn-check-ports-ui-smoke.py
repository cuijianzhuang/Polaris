# Browser regression for partial CDN checks and port-bound probe setup.
import copy, json, os, shutil
from datetime import datetime,timezone
from pathlib import Path
from urllib.parse import urlparse
from playwright.sync_api import sync_playwright,expect

scope={'__file__':str(Path(__file__).with_name('cdn-scheme-ui-smoke.py'))}
exec(Path(__file__).with_name('cdn-scheme-ui-smoke.py').read_text().rsplit('\nwith sync_playwright() as p:',1)[0],scope)
BaseFixture=scope['Fixture']
BASE=os.environ.get('POLARIS_WEB_TEST_URL','http://127.0.0.1:5187').rstrip('/')
OUT=Path(os.environ.get('POLARIS_CDN_ARTIFACTS','/tmp/polaris-cdn-ui'));OUT.mkdir(exist_ok=True,parents=True)
now=datetime.now(timezone.utc).isoformat()

class Fixture(BaseFixture):
 def __init__(self):
  super().__init__()
  self.profile.update(caddyFronting=False,nodeCheck=True,checkedAt=now,badIps=[],portChecks={'443':dict(badIps=[],allFailed=False,at=now),'8443':dict(badIps=[],allFailed=True,at=now)},reports=[dict(source='test443',scheme='https',port=443,at=now,ips=[dict(ip='104.16.1.1',latencyMs=10,loss=0)]),dict(source='test8443',scheme='https',port=8443,at=now,ips=[dict(ip='104.16.2.1',latencyMs=10,loss=0)])])
  self.inbound=copy.deepcopy(scope['scope']['fixtures']['ib'])
  self.inbound.update(tag='ws443',cdnCapable=True,port=443,protocol=dict(type='vless'),transport=dict(type='ws',path='/ws'),security=dict(type='tls',serverName='cdn.example.com'))
  self.verdict='partial'
 def route(self,route):
  req=route.request;path=urlparse(req.url).path.removeprefix('/api/v1')
  if path=='/cdn-profiles/cf/probes' and req.method=='POST':
   body=req.post_data_json;self.writes.append((req.method,path,body,req.headers))
   self.profile['probes']=[dict(source=body['source'],port=body['port'],tokenId='probe',prefix='plt_test',createdAt=now)]
   return route.fulfill(status=201,json=dict(body,token='plt_fixture',mirror=''))
  if path=='/inbounds':return route.fulfill(json=[self.inbound])
  if path=='/inbounds/i1/endpoints':return route.fulfill(json=[dict(id='ep',inboundId='i1',kind='cdn',cdnProfileId='cf',address='',port=0,sni='',host='',path='',remark='',warnings=[],target=dict(scheme='https',server='cdn.example.com',port=443,originPort=443,sni='cdn.example.com',host='cdn.example.com',path='/ws'))])
  if path=='/inbounds/i1/endpoints/ep/check':
   if self.verdict=='partial':return route.fulfill(json=dict(ok=False,partial=True,status=200,message='fixture: protocol handshake unverified',steps=['HTTP 200']))
   if self.verdict=='failed':return route.fulfill(json=dict(ok=False,status=403,message='fixture: Cloudflare WAF',steps=['HTTP 403']))
   return route.fulfill(json=dict(ok=True,status=101,message='fixture: transport only',steps=['HTTP 101']))
  return super().route(route)

with sync_playwright() as p:
 for engine in ['chromium','webkit']:
  args=dict(headless=True)
  if engine=='chromium':args.update(executable_path=os.environ.get('POLARIS_CHROMIUM_BIN') or shutil.which('chromium'),args=['--no-sandbox','--disable-dev-shm-usage'])
  browser=getattr(p,engine).launch(**args)
  for width in [390,1440]:
   for lang in ['zh','en']:
    f=Fixture();page=browser.new_page(viewport=dict(width=width,height=1000),locale='zh-CN' if lang=='zh' else 'en-US')
    page.set_default_timeout(10000)
    page.add_init_script("localStorage.setItem('polaris.token','fixture');localStorage.setItem('polaris.refresh','0');localStorage.setItem('polaris.lang',%s)"%json.dumps(lang))
    errors=[];page.on('pageerror',lambda e:errors.append(str(e)));page.route('**/api/v1/**',f.route)
    page.goto(BASE+'/chains')
    page.get_by_role('tab',name='CDN 配置档' if lang=='zh' else 'CDN profile',exact=True).click()
    page.get_by_text('各端口检测结果' if lang=='zh' else 'Checks by port',exact=True).click()
    expect(page.get_by_text('全部地址不可达，暂保留列表' if lang=='zh' else 'All addresses unreachable; retaining the list',exact=False)).to_be_visible()
    page.get_by_text('添加测速点（一键安装 cf-ip-probe）' if lang=='zh' else 'Add a speed-test point (one-line cf-ip-probe install)',exact=True).click()
    page.get_by_label('测速点名称' if lang=='zh' else 'point name',exact=True).fill('port8443')
    page.get_by_label('测速端口' if lang=='zh' else 'Test port',exact=True).select_option('8443')
    page.get_by_role('button',name='生成安装命令' if lang=='zh' else 'Generate install command',exact=True).click()
    expect(page.get_by_text('--args',exact=False).filter(has_text='-port 8443').first).to_be_visible()
    assert f.last('/cdn-profiles/cf/probes')[2]==dict(source='port8443',port=8443)
    assert page.evaluate('document.documentElement.scrollWidth <= innerWidth+1')
    page.goto(BASE+'/inbounds')
    page.get_by_text('ws443',exact=True).click()
    check=page.get_by_role('dialog').get_by_role('button',name='检测' if lang=='zh' else 'Check',exact=True)
    check.click()
    expect(page.get_by_text('检测部分完成' if lang=='zh' else 'Check partially completed',exact=True)).to_be_visible()
    expect(page.get_by_text('传输检测通过' if lang=='zh' else 'Transport check passed',exact=True)).to_have_count(0)
    page.get_by_text('检测部分完成' if lang=='zh' else 'Check partially completed',exact=True).scroll_into_view_if_needed()
    page.screenshot(path=str(OUT/f'{engine}-{width}-{lang}-partial.png'))
    f.verdict='failed';check.click()
    expect(page.get_by_text('检测未通过' if lang=='zh' else 'Check failed',exact=True)).to_be_visible()
    f.verdict='passed';check.click()
    expect(page.get_by_text('传输检测通过' if lang=='zh' else 'Transport check passed',exact=True)).to_be_visible()
    assert not errors,errors
    page.close();print(engine,width,lang,'port setup, per-port failure, partial/failed/transport verdicts OK',flush=True)
  browser.close()
