"""Agent-managed tunnel import, feature gating, rotation and cleanup UI."""
import copy,json,os,shutil,base64
from pathlib import Path
from urllib.parse import urlparse
from playwright.sync_api import sync_playwright,expect
scope={'__file__':str(Path(__file__).with_name('cdn-tunnel-ui-smoke.py'))}
exec(Path(scope['__file__']).read_text().rsplit('\nwith sync_playwright() as p:',1)[0],scope)
Base=scope['Fixture'];BASE=os.environ.get('POLARIS_WEB_TEST_URL','http://127.0.0.1:5187').rstrip('/')
OUT=Path(os.environ.get('POLARIS_CDN_ARTIFACTS','/tmp/polaris-cdn-ui'));OUT.mkdir(parents=True,exist_ok=True)
UUID='12345678-abcd-1234-abcd-123456789abc'
CREDENTIAL=json.dumps(dict(AccountTag='a'*32,TunnelID=UUID,TunnelSecret=base64.b64encode(b'not-real-private-test-credential-12345').decode()))
SECRET=json.loads(CREDENTIAL)['TunnelSecret']
class Fixture(Base):
 def __init__(self):
  super().__init__();self.supported=False
  self.profile.update(originMode='tunnel',tunnelId=UUID,ports=[443],caddyFronting=False,tunnelManaged=False,tunnelNodeId='',hasTunnelCredentials=False)
  self.state=dict(enabled=False,nodeId='',hasCredentials=False,nodeStatus='online',nodeName='Fixture',pending=False,running=False,error='',version='',cleanupConfirmed=True,activeEntries=1,ready=False)
 def route(self,route):
  req=route.request;path=urlparse(req.url).path.removeprefix('/api/v1')
  if path=='/nodes' and req.method=='GET':
   class WithFeatures:
    request=req
    def fulfill(proxy,**kwargs):
     payload=kwargs.get('json')
     if payload is None:payload=json.loads(kwargs.pop('body','[]'))
     for n in payload:n['agentFeatures']=['cloudflare-tunnel-v1'] if self.supported else []
     kwargs['json']=payload
     return route.fulfill(**kwargs)
   return super().route(WithFeatures())
  if path=='/cdn-profiles/cf/tunnel-agent':
   if req.method=='PUT':
    body=req.post_data_json;self.writes.append((req.method,path,body,req.headers))
    assert set(body)<=set(['enabled','nodeId','credentials'])
    assert not body['enabled'] or self.supported
    if body['enabled']:
     if not self.state['hasCredentials']:assert body.get('credentials')==CREDENTIAL
     self.profile.update(tunnelManaged=True,tunnelNodeId=body['nodeId'],hasTunnelCredentials=True)
     self.state.update(enabled=True,nodeId=body['nodeId'],hasCredentials=True,pending=True,cleanupConfirmed=False)
    else:
     assert 'credentials' not in body
     self.profile.update(tunnelManaged=False,hasTunnelCredentials=False)
     self.state.update(enabled=False,hasCredentials=False,pending=True,cleanupConfirmed=False,ready=False)
   return route.fulfill(json=self.state)
  return super().route(route)

with sync_playwright() as p:
 for engine in ['chromium','webkit']:
  browser=p.chromium.launch(executable_path=os.environ.get('POLARIS_CHROMIUM_BIN') or shutil.which('chromium'),args=['--no-sandbox','--disable-dev-shm-usage']) if engine=='chromium' else p.webkit.launch()
  for width in [390,1440]:
   for lang in ['zh','en']:
    f=Fixture();page=browser.new_page(viewport=dict(width=width,height=1000));page.set_default_timeout(10000);page.clock.install()
    page.add_init_script("localStorage.setItem('polaris.token','fixture');localStorage.setItem('polaris.refresh','0');localStorage.setItem('polaris.lang',%s)"%json.dumps(lang))
    errors=[];page.on('pageerror',lambda e,errors=errors:errors.append(str(e)));page.route('**/api/v1/**',f.route)
    page.goto(BASE+'/chains');page.get_by_role('tab',name='CDN 配置档' if lang=='zh' else 'CDN profile',exact=True).click()
    button=page.get_by_role('button',name='Agent 托管' if lang=='zh' else 'Agent management',exact=True);button.click();dialog=page.get_by_role('dialog')
    toggle=dialog.get_by_role('switch',name='由 Agent 托管 cloudflared' if lang=='zh' else 'Manage cloudflared with the Agent',exact=True);toggle.click()
    page.locator('#tunnel-agent-node').select_option(index=1)
    upload=dialog.get_by_label('导入隧道凭据' if lang=='zh' else 'Import tunnel credentials',exact=True)
    upload.set_input_files(dict(name='invalid.json',mimeType='application/json',buffer=b'not-json'))
    expect(dialog.get_by_role('alert')).to_contain_text('无法读取有效的 JSON' if lang=='zh' else 'Cannot read a valid JSON')
    upload.set_input_files(dict(name='tunnel.json',mimeType='application/json',buffer=CREDENTIAL.encode()))
    expect(dialog.get_by_role('alert')).to_have_count(0)
    save=dialog.get_by_role('button',name='保存' if lang=='zh' else 'Save',exact=True);expect(save).to_be_disabled()
    f.supported=True;page.clock.fast_forward(6000);expect(save).to_be_enabled()
    assert SECRET not in page.locator('body').inner_text() and SECRET not in page.evaluate('JSON.stringify(localStorage)')
    save.click();expect(dialog).to_have_count(0)
    assert f.writes[-1][2]['credentials']==CREDENTIAL and f.writes[-1][3].get('authorization')=='Bearer fixture'
    button.click();expect(dialog.get_by_role('status')).to_contain_text('配置待应用' if lang=='zh' else 'Configuration pending')
    f.state.update(pending=False,running=True,error='fixture: waiting for edge',version='2026.10.7');page.clock.fast_forward(6000)
    expect(dialog.get_by_role('status')).to_contain_text('fixture: waiting for edge')
    f.state.update(error='',ready=True);page.clock.fast_forward(6000)
    expect(dialog.get_by_role('status')).to_contain_text('节点隧道连接已就绪' if lang=='zh' else 'Node tunnel connections ready')
    assert SECRET not in page.locator('body').inner_text()
    page.screenshot(path=str(OUT/f'{engine}-{width}-{lang}-agent-managed.png'))
    save.click();expect(dialog).to_have_count(0);assert 'credentials' not in f.writes[-1][2]
    button.click();toggle.click();save.click();expect(dialog).to_have_count(0)
    assert f.writes[-1][2]['enabled'] is False and 'credentials' not in f.writes[-1][2]
    button.click();expect(dialog.get_by_role('status')).to_contain_text('等待 Agent 停止' if lang=='zh' else 'Waiting for the Agent to stop')
    f.state.update(pending=False,running=False,cleanupConfirmed=True);page.clock.fast_forward(6000)
    expect(dialog.get_by_role('status')).to_contain_text('Agent 已确认清理' if lang=='zh' else 'Cleanup confirmed by the Agent')
    assert page.evaluate('document.documentElement.scrollWidth<=innerWidth+1')
    assert not errors,(engine,width,lang,errors)
    page.close();print(engine,width,lang,'credential privacy, capability gate, readiness and cleanup acknowledgment OK',flush=True)
  browser.close()
