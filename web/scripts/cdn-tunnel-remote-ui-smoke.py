"""Remote-token one-click install, capability gating, privacy and cleanup."""
import json,os,shutil,base64
from pathlib import Path
from urllib.parse import urlparse
from playwright.sync_api import sync_playwright,expect
scope={'__file__':str(Path(__file__).with_name('cdn-tunnel-ui-smoke.py'))}
exec(Path(scope['__file__']).read_text().rsplit('\nwith sync_playwright() as p:',1)[0],scope)
Base=scope['Fixture'];BASE=os.environ.get('POLARIS_WEB_TEST_URL','http://127.0.0.1:5187').rstrip('/')
OUT=Path(os.environ.get('POLARIS_CDN_ARTIFACTS','/tmp/polaris-cdn-ui'));OUT.mkdir(parents=True,exist_ok=True)
UUID='12345678-abcd-1234-abcd-123456789abc'
TOKEN=base64.b64encode(json.dumps(dict(a='a'*32,t=UUID,s=base64.b64encode(b'not-real-private-test-credential-12345').decode())).encode()).decode()
class Fixture(Base):
 def __init__(self):
  super().__init__();self.remote=False
  self.profile.update(originMode='tunnel',tunnelId=UUID,ports=[443],caddyFronting=False,tunnelManaged=False,tunnelNodeId='',tunnelCredentialMode='',hasTunnelCredentials=False)
  self.state=dict(credentialMode='',enabled=False,nodeId='',hasCredentials=False,nodeStatus='online',nodeName='Fixture',pending=False,running=False,error='',version='',cleanupConfirmed=True,activeEntries=0,ready=False)
 def route(self,route):
  req=route.request;path=urlparse(req.url).path.removeprefix('/api/v1')
  if path=='/nodes' and req.method=='GET':
   class WithFeatures:
    request=req
    def fulfill(proxy,**kwargs):
     payload=kwargs.get('json')
     if payload is None:payload=json.loads(kwargs.pop('body','[]'))
     for n in payload:n['agentFeatures']=['cloudflare-tunnel-v1']+(['cloudflare-tunnel-token-v1'] if self.remote else [])
     kwargs['json']=payload
     return route.fulfill(**kwargs)
   return super().route(WithFeatures())
  if path=='/cdn-profiles/cf/tunnel-agent':
   if req.method=='PUT':
    body=req.post_data_json;self.writes.append((req.method,path,body,req.headers))
    assert set(body)<=set(['enabled','nodeId','credentialMode','token'])
    assert body['credentialMode']=='remote'
    if body['enabled']:
     assert self.remote
     if not self.state['hasCredentials']:assert body['token']==TOKEN
     self.profile.update(tunnelManaged=True,tunnelNodeId=body['nodeId'],hasTunnelCredentials=True,tunnelCredentialMode='remote')
     self.state.update(credentialMode='remote',enabled=True,nodeId=body['nodeId'],hasCredentials=True,pending=True,cleanupConfirmed=False)
    else:
     assert 'token' not in body
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
    page.locator('#tunnel-credential-mode').select_option('remote');page.locator('#tunnel-agent-node').select_option(index=1)
    field=dialog.get_by_label('Tunnel Token',exact=True);expect(field).to_have_attribute('type','password');field.fill(TOKEN)
    install=dialog.get_by_role('button',name='一键安装并启动' if lang=='zh' else 'Install and start',exact=True);expect(install).to_be_disabled()
    f.remote=True;page.clock.fast_forward(6000);expect(install).to_be_enabled()
    assert TOKEN not in page.locator('body').inner_text() and TOKEN not in page.evaluate('JSON.stringify(localStorage)')
    install.click();expect(dialog).to_have_count(0)
    assert f.writes[-1][2]['token']==TOKEN and f.writes[-1][3].get('authorization')=='Bearer fixture'
    button.click();expect(field).to_have_value('');expect(page.locator('#tunnel-credential-mode')).to_be_disabled()
    expect(dialog.get_by_role('status')).to_contain_text('配置待应用' if lang=='zh' else 'Configuration pending')
    f.state.update(pending=False,running=True,error='fixture: waiting for edge',version='2026.10.7');page.clock.fast_forward(6000)
    expect(dialog.get_by_role('status')).to_contain_text('fixture: waiting for edge')
    f.state.update(error='',ready=True);page.clock.fast_forward(6000)
    expect(dialog.get_by_role('status')).to_contain_text('节点隧道连接已就绪' if lang=='zh' else 'Node tunnel connections ready')
    assert TOKEN not in page.locator('body').inner_text()
    assert page.evaluate('document.documentElement.scrollWidth<=innerWidth+1')
    page.screenshot(animations='disabled',path=str(OUT/f'{engine}-{width}-{lang}-remote-token.png'))
    save=dialog.get_by_role('button',name='保存' if lang=='zh' else 'Save',exact=True);save.click();expect(dialog).to_have_count(0);assert 'token' not in f.writes[-1][2]
    button.click();toggle.click();save.click();expect(dialog).to_have_count(0)
    assert f.writes[-1][2]['enabled'] is False and 'token' not in f.writes[-1][2]
    button.click();expect(dialog.get_by_role('status')).to_contain_text('等待 Agent 停止' if lang=='zh' else 'Waiting for the Agent to stop')
    f.state.update(pending=False,running=False,cleanupConfirmed=True);page.clock.fast_forward(6000)
    expect(dialog.get_by_role('status')).to_contain_text('Agent 已确认清理' if lang=='zh' else 'Cleanup confirmed by the Agent')
    dialog.get_by_role('button',name='取消' if lang=='zh' else 'Cancel',exact=True).click();page.clock.fast_forward(300);expect(dialog).to_have_count(0)
    page.get_by_role('button',name='隧道回源配置' if lang=='zh' else 'Tunnel origin configuration',exact=True).click()
    expect(dialog).to_contain_text('Cloudflare')
    expect(dialog).to_contain_text('期望值' if lang=='zh' else 'expected values')
    expect(dialog).to_contain_text('http://127.0.0.1:18080')
    expect(dialog.locator('pre')).to_have_count(0)
    expect(dialog.get_by_role('button',name='复制配置' if lang=='zh' else 'Copy configuration',exact=True)).to_have_count(0)
    assert not errors,(engine,width,lang,errors)
    page.close();print(engine,width,lang,'remote token install, privacy, capability gate and cleanup OK',flush=True)
  browser.close()
