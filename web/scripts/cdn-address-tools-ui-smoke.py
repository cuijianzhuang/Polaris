"""Preferred address preview, blocking and layered diagnostics with real React."""
import copy,json,os,shutil
from time import monotonic
from datetime import datetime,timezone
from pathlib import Path
from urllib.parse import urlparse,parse_qs
from playwright.sync_api import sync_playwright,expect
scope={'__file__':str(Path(__file__).with_name('cdn-domains-ui-smoke.py'))}
exec(Path(scope['__file__']).read_text().rsplit('\nwith sync_playwright() as p:',1)[0],scope)
Base=scope['Fixture'];BASE=os.environ.get('POLARIS_WEB_TEST_URL','http://127.0.0.1:5187').rstrip('/')
OUT=Path(os.environ.get('POLARIS_CDN_ARTIFACTS','/tmp/polaris-cdn-ui'));OUT.mkdir(parents=True,exist_ok=True)
now=datetime.now(timezone.utc).isoformat()
class Fixture(Base):
 def __init__(self):
  super().__init__();self.profile.update(scheme='https',ports=[443,8443],expandPerIp=True,carrierSplit=True,regionSplit=True,blockedHosts=[],stalePolicy='source')
  self.checks={};self.failures=set();self.verdict='tls'
 def context(self,ep):
  return dict(endpointId=ep,inboundId='i1',label='Hong Kong / '+ep,transport='ws',warnings=[],target=dict(scheme=self.profile['scheme'],server='keep.example.com',port=8443 if ep=='ep8443' else self.profile['ports'][0],sni='cdn.example.com',host='cdn.example.com',path='/ws'))
 def route(self,route):
  req=route.request;url=urlparse(req.url);path=url.path.removeprefix('/api/v1');q=parse_qs(url.query)
  if path=='/cdn-profiles/cf/address-tools':
   ep=q.get('endpoint',['ep443'])[0] or 'ep443';ctx=self.context(ep);carrier=q.get('carrier',[''])[0];region=q.get('region',[''])[0]
   automatic=['104.16.1.1','104.16.1.2'] if ep=='ep443' else ['104.16.2.1']
   if region=='CN-44' and ep=='ep443':automatic=['104.16.4.1']
   pins=self.profile['pinnedHosts'];blocked=self.profile['blockedHosts']
   measured=automatic[1:] if carrier=='cm' and ep=='ep443' and len(automatic)>1 else automatic
   selected=([a for a in measured if a not in blocked and (ep,a) not in self.failures][:self.profile['ipTopN']]+[a for a in pins if a not in blocked and (ep,a) not in self.failures])
   items=[]
   for a in dict.fromkeys(automatic+pins+blocked):
    item=dict(address=a,type='domain' if a.endswith('.com') else 'ipv4',source='fixed' if a in pins else 'measurement',fixed=a in pins,blocked=a in blocked,blockKey=a if a in blocked else '',selected=a in selected,reason='blocked' if a in blocked else 'fixed' if a in pins and a in selected else 'selected' if a in selected else 'not_selected',sources=['Shanghai Telecom'],reportedAt=now,measurement=dict(latencyMs=20,loss=0,speed=12))
    if (ep,a) in self.failures:item.update(reason='check_failed' if a not in blocked else 'blocked',failedAt=now)
    if (ep,a) in self.checks:item['check']=self.checks[(ep,a)]
    items.append(item)
   return route.fulfill(json=dict(contexts=[self.context('ep443'),self.context('ep8443')],matchMode='carrier' if region=='CN-33' else 'region_carrier' if region and carrier else 'merged',context=ctx,items=items,selected=selected,scheme=self.profile['scheme'],port=ctx['target']['port'],carrier=carrier))
  if path=='/cdn-profiles/cf/address-policy':
   body=req.post_data_json;self.writes.append((req.method,path,body,req.headers));self.profile.update(body);return route.fulfill(json=dict(ok=True))
  if path=='/cdn-profiles/cf/check-address':
   body=req.post_data_json;self.writes.append((req.method,path,body,req.headers));target=copy.deepcopy(self.context(body['endpointId'])['target']);target['server']=body['address']
   if req.method=='DELETE':
    key=(body['endpointId'],body['address']);self.checks.pop(key,None);self.failures.discard(key);return route.fulfill(json=dict(ok=True))
   stages=[dict(kind='address_dns',state='passed',addresses=['104.16.1.1','2606:4700::1111']),dict(kind='tcp',state='passed'),dict(kind='tls',state='failed' if self.verdict=='tls' else 'passed'),dict(kind='http',state='not_tested' if self.verdict=='tls' else 'failed' if self.verdict=='waf' else 'passed',detail='HTTP 403' if self.verdict=='waf' else 'HTTP 101'),dict(kind='transport',state='passed' if self.verdict=='ok' else 'partial' if self.verdict=='partial' else 'failed')]
   result=dict(ok=self.verdict=='ok',partial=self.verdict=='partial',message='fixture: '+self.verdict,target=target,checkedAt=now,viewpoint='panel',businessVerified=False,stages=stages,warnings=[])
   key=(body['endpointId'],body['address'])
   if self.verdict=='ok':self.failures.discard(key)
   elif self.verdict!='partial':self.failures.add(key)
   self.checks[key]=result;return route.fulfill(json=result)
  return super().route(route)

class APIRequests:
 def __init__(self,page):
  self.pending=set();self.changed=monotonic()
  page.on('request',lambda req:self.update(req,True))
  page.on('requestfinished',lambda req:self.update(req,False))
  page.on('requestfailed',lambda req:self.update(req,False))
 def update(self,req,started):
  if '/api/v1/' not in req.url:return
  if started:self.pending.add(req)
  else:self.pending.discard(req)
  self.changed=monotonic()
 def settle_preview(self,page,card):
  # A policy write refreshes both the preview and its parent profile. The new
  # profile can start another preview fetch after the expected row is visible.
  # Track a fresh quiet interval each time: a document's initial networkidle
  # event alone does not guarantee that later SPA refreshes have completed.
  expect(card).to_have_attribute('aria-busy','false')
  self.changed=monotonic();deadline=self.changed+10
  while self.pending or monotonic()-self.changed<0.5:
   assert monotonic()<deadline,('API requests did not settle',[req.url for req in self.pending])
   page.wait_for_timeout(50) # Dispatch request events while polling the quiet interval.
  expect(card).to_have_attribute('aria-busy','false')

with sync_playwright() as p:
 for engine in ['chromium','webkit']:
  browser=p.chromium.launch(executable_path=os.environ.get('POLARIS_CHROMIUM_BIN') or shutil.which('chromium'),args=['--no-sandbox','--disable-dev-shm-usage']) if engine=='chromium' else p.webkit.launch()
  for width in [390,1440]:
   for lang in ['zh','en']:
    f=Fixture();page=browser.new_page(viewport=dict(width=width,height=1000));page.set_default_timeout(10000)
    requests=APIRequests(page)
    page.add_init_script("localStorage.setItem('polaris.token','fixture');localStorage.setItem('polaris.refresh','0');localStorage.setItem('polaris.lang',%s)"%json.dumps(lang))
    errors=[];page.on('pageerror',lambda e,errors=errors:errors.append(str(e)));page.route('**/api/v1/**',f.route)
    page.goto(BASE+'/chains');page.get_by_role('tab',name='CDN 配置档' if lang=='zh' else 'CDN profile',exact=True).click()
    card=page.get_by_role('region',name='优选地址与订阅预览' if lang=='zh' else 'Preferred addresses and subscription preview')
    expect(card).to_contain_text('104.16.1.1');expect(card).to_contain_text('keep.example.com')
    row=card.locator('[data-address="104.16.1.1"]')
    row.get_by_role('button',name='禁用' if lang=='zh' else 'Disable',exact=True).click()
    expect(row).to_contain_text('已禁用' if lang=='zh' else 'Disabled')
    requests.settle_preview(page,card)
    page.reload();page.get_by_role('tab',name='CDN 配置档' if lang=='zh' else 'CDN profile',exact=True).click()
    expect(row).to_contain_text('已禁用' if lang=='zh' else 'Disabled')
    row.get_by_role('button',name='恢复' if lang=='zh' else 'Restore',exact=True).click()
    expect(row).to_contain_text('按当前策略选中' if lang=='zh' else 'Selected by current policy')
    carrier=card.get_by_label('预览运营商' if lang=='zh' else 'Preview carrier');carrier.select_option('cm')
    expect(card.locator('[data-address="104.16.1.2"]')).to_contain_text('按当前策略选中' if lang=='zh' else 'Selected by current policy')
    region=card.get_by_label('预览地区' if lang=='zh' else 'Preview region');region.select_option('CN-44')
    expect(card.locator('[data-address="104.16.4.1"]')).to_be_visible()
    expect(card).to_contain_text('已匹配同地区、同运营商' if lang=='zh' else 'Matched region and carrier')
    region.select_option('CN-33')
    expect(card).to_contain_text('本地区无匹配结果，回退同运营商其他地区' if lang=='zh' else 'No regional match; using this carrier in other regions')
    region.select_option('')
    entry=card.get_by_label('用于检查和预览的入口' if lang=='zh' else 'Entry for checks and preview');entry.select_option('ep8443')
    preferred=card.locator('[data-address="104.16.2.1"]');expect(preferred).to_be_visible()
    check=preferred.get_by_role('button',name='检测' if lang=='zh' else 'Check',exact=True)
    check.click();result=card.get_by_role('status');expect(result).to_contain_text('fixture: tls')
    expect(result).to_contain_text('2606:4700::1111');expect(result).to_contain_text('8443')
    assert f.last('/cdn-profiles/cf/check-address')[2]==dict(endpointId='ep8443',address='104.16.2.1')
    expect(preferred).to_contain_text('检测失败，已排除' if lang=='zh' else 'Check failed; excluded')
    requests.settle_preview(page,card)
    page.reload();page.get_by_role('tab',name='CDN 配置档' if lang=='zh' else 'CDN profile',exact=True).click()
    entry.select_option('ep8443');expect(preferred).to_contain_text('检测失败，已排除' if lang=='zh' else 'Check failed; excluded')
    entry.select_option('ep443');expect(card.locator('[data-address="104.16.1.1"]')).to_contain_text('按当前策略选中' if lang=='zh' else 'Selected by current policy')
    entry.select_option('ep8443');expect(preferred).to_contain_text('检测失败，已排除' if lang=='zh' else 'Check failed; excluded')
    f.verdict='waf';check.click();expect(result).to_contain_text('HTTP 403')
    f.verdict='partial';check.click();expect(result).to_contain_text('检测部分完成' if lang=='zh' else 'Check partially completed')
    expect(preferred).to_contain_text('检测失败，已排除' if lang=='zh' else 'Check failed; excluded')
    f.verdict='ok';check.click();expect(result).to_contain_text('传输检测通过' if lang=='zh' else 'Transport check passed')
    expect(preferred).to_contain_text('按当前策略选中' if lang=='zh' else 'Selected by current policy')
    expect(card).to_contain_text('不代表客户端网络或代理认证通过' if lang=='zh' else 'do not verify the client network or proxy authentication')
    card.get_by_role('button',name='地址策略' if lang=='zh' else 'Address policy',exact=True).click()
    dialog=page.get_by_role('dialog');dialog.get_by_label('测速结果过期后' if lang=='zh' else 'When measurements expire').select_option('keep')
    dialog.get_by_label('禁用地址' if lang=='zh' else 'Blocked addresses').fill('keep.example.com')
    dialog.get_by_role('button',name='保存' if lang=='zh' else 'Save',exact=True).click();expect(dialog).to_have_count(0)
    assert f.last('/cdn-profiles/cf/address-policy')[2]==dict(blockedHosts=['keep.example.com'],stalePolicy='keep')
    expect(card.locator('[data-address="keep.example.com"]')).to_contain_text('已禁用' if lang=='zh' else 'Disabled')
    f.verdict='tls';check.click()
    expect(card).to_contain_text('当前入口的候选已被排除' if lang=='zh' else "This entry's candidates have been excluded")
    assert page.evaluate('document.documentElement.scrollWidth<=innerWidth+1')
    card.scroll_into_view_if_needed();page.screenshot(path=str(OUT/f'{engine}-{width}-{lang}-tools.png'))
    preferred.get_by_role('button',name='重置检测' if lang=='zh' else 'Reset check',exact=True).click()
    expect(preferred).to_contain_text('未检测' if lang=='zh' else 'Not checked')
    expect(preferred).to_contain_text('按当前策略选中' if lang=='zh' else 'Selected by current policy')
    requests.settle_preview(page,card)
    page.close()
    assert not errors,(engine,width,lang,errors)
    print(engine,width,lang,'preview/carrier/port, blacklist persistence, layered verdicts and expiry policy OK',flush=True)
  browser.close()
