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
with sync_playwright() as p:
 browser=p.chromium.launch(executable_path=CHROMIUM,headless=True,args=['--no-sandbox','--disable-dev-shm-usage'])
 for width in [390,1440]:
  page,f,errors=open_page(browser,'/inbounds',width)
  page.locator('td.rt-head').click()
  expect(page.get_by_label('Snell 服务端版本',exact=True)).to_have_value('5')
  expect(page.get_by_text('v5 使用 v4 客户端协议；当前仅向 sing-box 1.14+ 订阅输出，其他客户端会过滤此节点。',exact=True)).to_be_visible()
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
 # An old kernel must keep the protocol disabled.
 caps[0]['version']='1.13.5';caps[0]['combos']=[dict(protocol='vless',transport='tcp',security='none')]
 page,f,errors=open_page(browser,'/inbounds',390);page.locator('td.rt-head').click()
 expect(page.get_by_role('button',name='Snell',exact=False)).to_be_disabled()
 assert not errors,errors;page.close();browser.close();print('old core disables Snell option OK',flush=True)
