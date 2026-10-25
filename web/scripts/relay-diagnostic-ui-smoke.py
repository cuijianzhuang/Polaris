# Result semantics and support gating; native proxy verification uses Go tests.
import ast
from pathlib import Path
source=Path(__file__).with_name('operations-ui-smoke.py').read_text();tree=ast.parse(source)
exec(compile(ast.Module(body=[n for n in tree.body if not isinstance(n,ast.With)],type_ignores=[]),str(Path(__file__).with_name('operations-ui-smoke.py')),'exec'))

reason='真实业务诊断目前仅支持 Xray VLESS；此限制不影响当前协议的中转创建和转发'
class DiagnosticFixture(Fixture):
 def __init__(self):
  super().__init__();self.inbound=copy.deepcopy(ib);self.relay=copy.deepcopy(relay);self.relay['businessDiagnostic']=dict(supported=True)
  self.network=dict(state='partial',businessVerified=False,checks=[dict(kind='deployment',state='passed'),dict(kind='entry_tls_handshake',state='passed',latencyMs=327),dict(kind='hop_tcp',state='passed'),dict(kind='landing_egress_http',state='passed'),dict(kind='proxy_business',state='not_tested')])
  self.business=dict(state='unsupported',businessVerified=False,message=reason)
 def route(self,route):
  req=route.request;path=urlparse(req.url).path.removeprefix('/api/v1')
  if path=='/inbounds':result=[self.inbound]
  elif path=='/inbounds/i1/relays':result=[self.relay]
  elif path.endswith('/diagnose/business') or path.endswith('/diagnose'):
   self.writes.append((req.method,path,req.post_data_json,req.headers));result=self.business if path.endswith('/business') else self.network
  else:return super().route(route)
  route.fulfill(status=200,content_type='application/json',body=json.dumps(result))

def txt(lang,zh,en):return zh if lang=='zh' else en
with sync_playwright() as p:
 browser=p.chromium.launch(executable_path=CHROMIUM,headless=True,args=['--no-sandbox','--disable-dev-shm-usage'])
 for width in [390,1440]:
  for lang in ['zh','en']:
   f=DiagnosticFixture();page=browser.new_page(viewport=dict(width=width,height=1100));page.set_default_timeout(10000)
   page.add_init_script("localStorage.setItem('polaris.token','fixture');localStorage.setItem('polaris.lang',%s);localStorage.setItem('polaris.refresh','0')"%json.dumps(lang))
   errors=[];page.on('pageerror',lambda e:errors.append(str(e)));page.route('**/api/v1/**',f.route)
   def open_manager():
    page.goto(BASE_URL+'/inbounds');page.get_by_role('button',name=txt(lang,'中转','Relay'),exact=True).click()
   def close_result():page.keyboard.press('Escape')
   def network():page.get_by_role('button',name=txt(lang,'网络诊断','Network diagnostic'),exact=True).click();return page.get_by_role('dialog',name=txt(lang,'线路诊断结果','Relay diagnostic result'),exact=True)
   def business():
    page.get_by_role('button',name=txt(lang,'真实业务诊断','Real service diagnostic'),exact=True).click();page.get_by_role('button',name=txt(lang,'确定','OK'),exact=True).click();return page.get_by_role('dialog',name=txt(lang,'线路诊断结果','Relay diagnostic result'),exact=True)
   open_manager();dialog=network()
   expect(dialog.get_by_text(txt(lang,'已执行的网络检查通过','The network checks that ran passed'),exact=True)).to_be_visible()
   expect(dialog.get_by_text(txt(lang,'本次仅检查网络，代理业务未测试；未测试不代表业务失败。','This checked the network only. Proxy business was not tested; untested does not mean failed.'),exact=True)).to_be_visible()
   expect(dialog.get_by_text(txt(lang,'代理业务验证','Proxy business verification'),exact=True)).to_be_visible()
   expect(dialog.locator('.ops-summary').last).to_contain_text(txt(lang,'未测试','Untested'))
   expect(dialog.locator('.note.warn')).to_have_count(0);no_overflow(page)
   dialog.screenshot(path=str(OUT/f'relay-network-result-{lang}-{width}.png'));close_result()
   f.network['state']='failed';f.network['checks'][1]['state']='failed';dialog=network()
   expect(dialog.get_by_text(txt(lang,'网络诊断发现异常','Network diagnostics found a problem'),exact=True)).to_be_visible();expect(dialog.locator('.note.warn')).to_have_count(1);close_result()
   # A server-side capability change between list and click is neutral, not a failed business test.
   dialog=business();expect(dialog.get_by_text(txt(lang,'当前入站暂不支持真实业务诊断','Real business diagnostics are not supported for this inbound'),exact=True)).to_be_visible();expect(dialog.locator('.note.warn')).to_have_count(0);close_result()
   f.business=dict(state='not_ready',businessVerified=False);dialog=business();expect(dialog.get_by_text(txt(lang,'真实业务诊断尚未就绪','Real business diagnostics are not ready'),exact=True)).to_be_visible();expect(dialog.locator('.note.warn')).to_have_count(0);close_result()
   f.business=dict(state='failed',businessVerified=False);dialog=business();expect(dialog.get_by_text(txt(lang,'真实业务诊断未通过','Real business diagnostics did not pass'),exact=True)).to_be_visible();expect(dialog.locator('.note.warn')).to_have_count(1);close_result()
   f.business=dict(state='passed',businessVerified=True);dialog=business();expect(dialog.get_by_text(txt(lang,'真实业务已验证','Real service verified'),exact=True)).to_be_visible();close_result()
   f.inbound['core']='sing-box';f.relay['businessDiagnostic']=dict(supported=False,reason=reason)
   open_manager();button=page.get_by_role('button',name=txt(lang,'真实业务诊断','Real service diagnostic'),exact=True);expect(button).to_be_disabled()
   expect(page.get_by_text(reason if lang=='zh' else 'Real business diagnostics currently support Xray VLESS only. This does not restrict relay creation or forwarding for the current protocol.',exact=True)).to_be_visible()
   expect(page.get_by_role('button',name=txt(lang,'网络诊断','Network diagnostic'),exact=True)).to_be_enabled()
   assert not errors,errors;page.close();print(width,lang,'network pass/failure, untested business, unsupported/not-ready neutrality, actual business failure/success and sing-box gating passed',flush=True)
 browser.close()
