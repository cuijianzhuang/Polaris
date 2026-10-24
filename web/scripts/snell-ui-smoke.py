# Mocked UI regression only. Native auth/accounting are Go integration tests.
import ast
from pathlib import Path
source=Path(__file__).with_name('operations-ui-smoke.py').read_text()
tree=ast.parse(source)
exec(compile(ast.Module(body=[n for n in tree.body if not isinstance(n,ast.With)],type_ignores=[]),str(Path(__file__).with_name('operations-ui-smoke.py')),'exec'))
node['cores']=[dict(id='c1',nodeId='n1',type='singbox',version='1.14.4',status='running',installedVersions=[])]
node['defaultCore']='singbox'
ib.update(core='singbox',protocol=dict(type='snell',snellVersion=5),transport=dict(type='tcp'),security=dict(type='none'),outboundIPStrategy='auto',relayOnly=False,relayAccess=dict(enabled=False,proxyProtocol=0))
caps[:]=[dict(core='singbox',version='1.14.4',combos=[dict(protocol='snell',transport='tcp',security='none'),dict(protocol='vless',transport='tcp',security='none')],features=['outbound-ip'],hotUsers=False)]
class SnellFixture(Fixture):
 def route(self,route):
  path=urlparse(route.request.url).path.removeprefix('/api/v1')
  if path=='/users':
   route.fulfill(status=200,content_type='application/json',body=json.dumps(dict(items=[dict(id='user-one',username='Alice')])));return
  return super().route(route)

def dedicated_page(browser,width):
 f=SnellFixture();page=browser.new_page(viewport=dict(width=width,height=1000),locale='zh-CN',timezone_id='Asia/Shanghai')
 page.set_default_timeout(10000);page.add_init_script("localStorage.setItem('polaris.token','fixture');localStorage.setItem('polaris.lang','zh');localStorage.setItem('polaris.refresh','0')")
 page.route('**/api/v1/**',f.route);errors=[];page.on('pageerror',lambda e:errors.append(str(e)));page.goto(BASE_URL+'/inbounds')
 return page,f,errors

with sync_playwright() as p:
 browser=p.chromium.launch(executable_path=CHROMIUM,headless=True,args=['--no-sandbox','--disable-dev-shm-usage'])
 for width in [390,1440]:
  page,f,errors=open_page(browser,'/inbounds',width)
  page.locator('td.rt-head').click()
  expect(page.get_by_label('Snell 服务端版本',exact=True)).to_have_value('5')
  expect(page.get_by_text('v5 输出 v4 客户端协议。Surge 使用独立 PSK 模式；v6 需 Surge iOS 5.20+ 或 Mac 6.7+，并仍需实际客户端验收。',exact=True)).to_be_visible()
  page.get_by_label('Snell 混淆',exact=True).select_option('http')
  page.get_by_label('Snell 服务端版本',exact=True).select_option('6')
  expect(page.get_by_label('Snell 混淆',exact=True)).to_have_count(0)
  page.get_by_label('流量整形',exact=True).select_option('unshaped')
  page.get_by_role('button',name='保存并下发',exact=True).click()
  expect(page.get_by_role('button',name='保存并下发',exact=True)).to_be_enabled()
  saved=f.last('/inbounds/i1')[2]
  assert saved['protocol']==dict(type='snell',snellVersion=6,snellMode='unshaped'),saved['protocol']
  assert saved['security']['type']=='none' and saved['transport']['type']=='tcp'
  no_overflow(page);assert not errors,errors;page.screenshot(path=str(OUT/f'snell-editor-{width}.png'));page.close()
  print(width,'Snell mode editor, compatibility hint, no obsolete obfs in v6, save and layout OK',flush=True)
 for width in [390,1440]:
  page,f,errors=dedicated_page(browser,width);page.locator('td.rt-head').click()
  page.get_by_label('Snell 授权模式',exact=True).select_option('psk')
  page.get_by_label('Snell 专属用户',exact=True).select_option('user-one')
  page.get_by_label('Snell 服务端版本',exact=True).select_option('6')
  expect(page.locator('#f-snell-mode option[value="unsafe-raw"]')).to_have_count(0)
  page.get_by_label('流量整形',exact=True).select_option('unshaped')
  page.get_by_role('button',name='保存并下发',exact=True).click();expect(page.get_by_role('button',name='保存并下发',exact=True)).to_be_enabled()
  saved=f.last('/inbounds/i1')[2]['protocol'];assert saved['snellAuth']=='psk' and saved['snellUserId']=='user-one' and saved['snellMode']=='unshaped',saved
  no_overflow(page);assert not errors,errors;page.close();print(width,'dedicated PSK owner selection and encrypted-mode save OK',flush=True)
 # An old kernel must keep the protocol disabled.
 caps[0]['version']='1.13.5';caps[0]['combos']=[dict(protocol='vless',transport='tcp',security='none')]
 page,f,errors=open_page(browser,'/inbounds',390);page.locator('td.rt-head').click()
 expect(page.get_by_role('button',name='Snell',exact=False)).to_be_disabled()
 assert not errors,errors;page.close();browser.close();print('old core disables Snell option OK',flush=True)
