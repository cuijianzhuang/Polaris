# Match the real preview API contract; mocks must reject tunnel-only fields in l4.
import ast
from pathlib import Path
source=Path(__file__).with_name('operations-ui-smoke.py').read_text();tree=ast.parse(source)
exec(compile(ast.Module(body=[n for n in tree.body if not isinstance(n,ast.With)],type_ignores=[]),str(Path(__file__).with_name('operations-ui-smoke.py')),'exec'))
ib.update(core='singbox',protocol=dict(type='anytls'),transport=dict(type='tcp'),security=dict(type='tls'),tag='AnyTLS target')
class RelayCreateFixture(Fixture):
 def __init__(self):super().__init__();self.fail=True;self.hold=False;self.pending=None;self.created=False
 def route(self,route):
  req=route.request;path=urlparse(req.url).path.removeprefix('/api/v1')
  if path=='/inbounds/i1/relays':result=[dict(relay,name='Created relay',state='pending')] if self.created else []
  elif path=='/inbounds/i1/relays/batch/preview':
   body=req.post_data_json;self.writes.append((req.method,path,body,req.headers))
   if body['mode']=='l4' and body.get('transport'):
    route.fulfill(status=400,content_type='application/json',body=json.dumps(dict(error='原样转发不需要隧道传输参数')));return
   if body['mode']=='tunnel':assert body['transport'] in ['tcp','ws'],body
   if self.fail:
    self.fail=False;route.fulfill(status=400,content_type='application/json',body=json.dumps(dict(error='Fixture preview failure')));return
   result=dict(planToken='STRICT_FROZEN_PLAN',batchId='new-batch',expiresAt=(datetime.now(timezone.utc)+timedelta(minutes=5)).isoformat(),warnings=[],entries=[dict(chain=dict(id='new-relay',name='Preview path',hops=[]),address='203.0.113.2',port=20443,ports=[port])])
   if self.hold:self.hold=False;self.pending=(route,result);return
  elif path=='/inbounds/i1/relays/batch':
   body=req.post_data_json;self.writes.append((req.method,path,body,req.headers));assert body==dict(planToken='STRICT_FROZEN_PLAN'),body;self.created=True;result=dict(batchId='new-batch',chains=[])
   route.fulfill(status=201,content_type='application/json',body=json.dumps(result));return
  else:return super().route(route)
  route.fulfill(status=200,content_type='application/json',body=json.dumps(result))

with sync_playwright() as p:
 browser=p.chromium.launch(executable_path=CHROMIUM,headless=True,args=['--no-sandbox','--disable-dev-shm-usage'])
 for width in [390,1440]:
  for lang in ['zh','en']:
   for mode in ['l4','tunnel']:
    f=RelayCreateFixture();page=browser.new_page(viewport=dict(width=width,height=1000),locale='zh-CN' if lang=='zh' else 'en-US');page.set_default_timeout(10000)
    page.add_init_script("localStorage.setItem('polaris.token','fixture');localStorage.setItem('polaris.lang',%s);localStorage.setItem('polaris.refresh','0')"%json.dumps(lang))
    page.route('**/api/v1/**',f.route);errors=[];page.on('pageerror',lambda e:errors.append(str(e)));page.goto(BASE_URL+'/inbounds')
    page.get_by_role('button',name='中转' if lang=='zh' else 'Relay',exact=True).click()
    page.get_by_role('button',name='一键配置中转' if lang=='zh' else 'Configure relay',exact=False).click()
    dialog=page.get_by_role('dialog',name='一键配置中转' if lang=='zh' else 'Configure relay',exact=True)
    selector=dialog.get_by_label('中转方式' if lang=='zh' else 'Relay mode',exact=True);selector.select_option(mode)
    if mode=='tunnel':dialog.get_by_label('隧道传输' if lang=='zh' else 'Tunnel transport',exact=True).select_option('ws')
    dialog.get_by_role('checkbox',name='Tokyo · 在线' if lang=='zh' else 'Tokyo · Online',exact=True).check()
    preview=dialog.get_by_role('button',name='预览配置' if lang=='zh' else 'Preview configuration',exact=True)
    create=dialog.get_by_role('button',name='确认创建并下发' if lang=='zh' else 'Confirm and deploy',exact=True)
    preview.click();expect(dialog.get_by_role('alert')).to_have_text('Fixture preview failure');expect(create).to_have_count(0)
    dialog.get_by_label('备注' if lang=='zh' else 'Remark',exact=True).fill('Retry');expect(dialog.get_by_role('alert')).to_have_count(0)
    f.hold=True;preview.click();expect(selector).to_be_disabled();expect(dialog.get_by_label('线路名称' if lang=='zh' else 'Relay name',exact=True)).to_be_disabled()
    pending,result=f.pending;pending.fulfill(status=200,content_type='application/json',body=json.dumps(result))
    expect(create).to_be_enabled();body=f.last('/inbounds/i1/relays/batch/preview')[2]
    assert body['mode']==mode and body['entries']==[dict(nodeId='n2')],body
    assert body.get('transport')==('ws' if mode=='tunnel' else None),body
    # Switching from a tunnel back to default forwarding discards its old plan.
    if mode=='tunnel':
     selector.select_option('l4');expect(create).to_have_count(0);preview.click();expect(create).to_be_enabled();assert 'transport' not in f.last('/inbounds/i1/relays/batch/preview')[2]
    no_overflow(page);page.screenshot(path=str(OUT/f'relay-create-{mode}-{lang}-{width}.png'),full_page=True,animations='disabled',style='.toasts { visibility: hidden; }')
    create.click();expect(dialog).to_have_count(0);expect(page.get_by_text('Created relay',exact=True)).to_be_visible();assert f.last('/inbounds/i1/relays/batch')[2]==dict(planToken='STRICT_FROZEN_PLAN')
    assert not errors,errors;page.close();print(width,lang,mode,'strict preview contract, inline errors, busy fields, mode reset and create token passed',flush=True)
 browser.close()
