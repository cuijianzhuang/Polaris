"""Cloudflare Tunnel settings and private WS origin creation in real browsers."""
import copy,json,os,shutil
from pathlib import Path
from urllib.parse import urlparse
from playwright.sync_api import sync_playwright,expect
scope={'__file__':str(Path(__file__).with_name('cdn-scheme-ui-smoke.py'))}
exec(Path(scope['__file__']).read_text().rsplit('\nwith sync_playwright() as p:',1)[0],scope)
Base=scope['Fixture'];BASE=os.environ.get('POLARIS_WEB_TEST_URL','http://127.0.0.1:5187').rstrip('/')
OUT=Path(os.environ.get('POLARIS_CDN_ARTIFACTS','/tmp/polaris-cdn-ui'));OUT.mkdir(parents=True,exist_ok=True)
UUID='12345678-abcd-1234-abcd-123456789abc'

class Fixture(Base):
 def __init__(self):super().__init__();self.config_error=False
 def route(self,route):
  req=route.request;path=urlparse(req.url).path.removeprefix('/api/v1')
  if path=='/cdn-profiles/cf/tunnel-config':
   if self.config_error:return route.fulfill(status=400,json=dict(error='fixture: add an enabled WS entry first'))
   config='tunnel: '+UUID+'\ncredentials-file: /etc/cloudflared/'+UUID+'.json\ningress:\n  - hostname: cdn.example.com\n    path: ^/ws$\n    service: http://127.0.0.1:18080\n  - service: http_status:404\n'
   return route.fulfill(json=dict(nodeId='n1',tunnelId=UUID,dnsTarget=UUID+'.cfargotunnel.com',config=config,routes=[dict(hostname='cdn.example.com',path='^/ws$',service='http://127.0.0.1:18080',originRequest=dict(httpHostHeader='cdn.example.com')),dict(service='http_status:404')]))
  return super().route(route)

with sync_playwright() as p:
 for engine in ['chromium','webkit']:
  browser=p.chromium.launch(executable_path=os.environ.get('POLARIS_CHROMIUM_BIN') or shutil.which('chromium'),args=['--no-sandbox','--disable-dev-shm-usage']) if engine=='chromium' else p.webkit.launch()
  for width in [390,1440]:
   for lang in ['zh','en']:
    f=Fixture();page=browser.new_page(viewport=dict(width=width,height=1000));page.set_default_timeout(10000)
    page.add_init_script("localStorage.setItem('polaris.token','fixture');localStorage.setItem('polaris.refresh','0');localStorage.setItem('polaris.lang',%s)"%json.dumps(lang))
    errors=[];page.on('pageerror',lambda e,errors=errors:errors.append(str(e)));page.route('**/api/v1/**',f.route)
    page.goto(BASE+'/chains');page.get_by_role('tab',name='CDN 配置档' if lang=='zh' else 'CDN profile',exact=True).click()
    page.get_by_role('button',name='编辑' if lang=='zh' else 'Edit',exact=True).last.click()
    dialog=page.get_by_role('dialog');page.locator('#cdn-origin-mode').select_option('tunnel')
    page.locator('#cdn-tunnel-id').fill(UUID)
    expect(page.locator('#cdn-scheme')).to_have_value('https');expect(page.locator('#cdn-scheme')).to_be_disabled()
    expect(page.locator('#cdn-ports')).to_have_value('443');expect(page.locator('#cdn-ports')).to_be_disabled()
    expect(page.locator('#cdn-origin-tls')).to_have_count(0)
    expect(dialog.get_by_role('switch',name='Caddy',exact=True)).to_have_count(0)
    dialog.get_by_role('button',name='保存' if lang=='zh' else 'Save',exact=True).click();expect(dialog).to_have_count(0)
    saved=f.last('/cdn-profiles/cf')[2]
    assert saved['originMode']=='tunnel' and saved['tunnelId']==UUID and saved['scheme']=='https' and saved['ports']==[443] and not saved['caddyFronting'],saved
    config_button=page.get_by_role('button',name='隧道回源配置' if lang=='zh' else 'Tunnel origin configuration',exact=True)
    config_button.click();expect(dialog).to_contain_text('http_status:404');expect(dialog).to_contain_text(UUID+'.cfargotunnel.com')
    expect(dialog).to_contain_text('http://127.0.0.1:18080')
    assert page.evaluate('document.documentElement.scrollWidth<=innerWidth+1')
    page.screenshot(path=str(OUT/f'{engine}-{width}-{lang}-tunnel-config.png'))
    dialog.get_by_role('button',name='关闭' if lang=='zh' else 'Close',exact=True).last.click();expect(dialog).to_have_count(0)
    f.config_error=True;config_button.click();expect(dialog.get_by_role('alert')).to_contain_text('fixture: add an enabled WS entry first')
    f.config_error=False;dialog.get_by_role('button',name='重试' if lang=='zh' else 'Retry',exact=True).click();expect(dialog.get_by_role('alert')).to_have_count(0);expect(dialog).to_contain_text('http_status:404')
    dialog.get_by_role('button',name='关闭' if lang=='zh' else 'Close',exact=True).last.click()
    page.reload();page.get_by_role('tab',name='CDN 配置档' if lang=='zh' else 'CDN profile',exact=True).click();expect(config_button).to_be_visible()
    page.goto(BASE+'/inbounds');page.get_by_role('button',name='+ 新建入站' if lang=='zh' else '+ New inbound',exact=True).click()
    page.get_by_role('button',name='一键配置 CDN' if lang=='zh' else 'Set up for CDN',exact=True).first.click()
    expect(page.locator('#ib-listen')).to_have_value('127.0.0.1')
    page.locator('#ib-tag').fill('tunnel-fixture')
    dialog.get_by_role('button',name='保存并下发' if lang=='zh' else 'Save and push',exact=True).click();expect(dialog).to_have_count(0)
    assert f.created['listen']=='127.0.0.1' and f.created['transport']['type']=='ws' and f.created['security']['type']=='none' and f.created['cdnOnly'] and f.created['cdnProfileId']=='cf',f.created
    assert not errors,(engine,width,lang,errors)
    page.close();print(engine,width,lang,'tunnel settings, config export/retry and local WS origin OK',flush=True)
  browser.close()
