"""Settings reads must not masquerade as off; retries and failed saves keep state."""
import json, os, shutil
from pathlib import Path
from urllib.parse import urlparse
from playwright.sync_api import sync_playwright, expect

source=Path(__file__).with_name('settings-workflow-smoke.py').resolve()
scope={'__file__':str(source)}
exec(source.read_text().split('\nwith sync_playwright() as p:')[0],scope)
Base=scope['Fixture']
BASE=os.environ.get('POLARIS_WEB_TEST_URL','http://127.0.0.1:5187').rstrip('/')

class Fixture(Base):
 def __init__(self):
  super().__init__()
  self.settings.update(dict(tg_enabled='true',tg_notify_users='false',tg_daily_report='false',tg_token_set=True,remind_usage_pct='0',tg_daily_report_hour='0',tickets_enabled='true',shop_enabled='true',sub_enabled='true',sub_show_bot='',sub_show_rate='false',probe_enabled='true',status_page='false',audit_changes='true',geodata_cron='0 4 * * *',ddns_auto_address='false',converter_for_users='true',mail_notify_users='',mail_notify_admin='false'))
  self.block=True;self.pending=[];self.read_error=False;self.malformed=False
  self.runtime=False;self.runtime_error=False;self.hold_runtime=False;self.pending_runtime=[];self.runtime_calls=0
  self.hold_write=False;self.pending_write=[];self.writes=[];self.reject_write=False
 def read(self,route):
  if self.read_error:return route.fulfill(status=503,json=dict(error='Synthetic settings read failure'))
  return route.fulfill(json=[] if self.malformed else self.settings)
 def release(self):
  self.block=False
  for route in self.pending:self.read(route)
  self.pending=[]
 def status(self,route):
  if self.runtime_error:return route.fulfill(status=503,json=dict(error='Synthetic runtime read failure'))
  return route.fulfill(json=dict(running=self.runtime,username='fixture_bot',error=''))
 def commit(self,route):
  if self.reject_write:return route.fulfill(status=503,json=dict(error='Synthetic save failure'))
  for key,value in route.request.post_data_json.items():
   if key=='tg_token':
    if value:self.settings['tg_token_set']=True
   else:self.settings[key]=value
  return route.fulfill(json=self.settings)
 def route(self,route):
  path=urlparse(route.request.url).path
  if path=='/api/v1/settings':
   if route.request.method=='PUT':
    self.writes.append(route.request.post_data_json)
    if self.hold_write:self.pending_write.append(route);return
    return self.commit(route)
   if self.block:self.pending.append(route);return
   return self.read(route)
  if path=='/api/v1/telegram':
   self.runtime_calls+=1
   if self.hold_runtime:self.pending_runtime.append(route);return
   return self.status(route)
  if path=='/api/v1/telegram/admins':return route.fulfill(json=dict(admins=[],boundUsers=0))
  if path=='/api/v1/tickets':return route.fulfill(json=dict(items=[],counts={},enabled=True))
  if path=='/api/v1/audit-logs':return route.fulfill(json=dict(items=[],total=0))
  if path=='/api/v1/probe':return route.fulfill(json=dict(nodes=[],targets=[],at='2026-11-05T00:00:00Z',probeEnabled=True,statusPage=False))
  if path=='/api/v1/cdn-shodan':return route.fulfill(json=dict(enabled=False,keySet=False,query='',pages=1,count=0,at='',defaultQuery='',error=''))
  if path=='/api/v1/orders':return route.fulfill(json=dict(items=[],total=0,status=dict(enabled=True,botRunning=False,bot='fixture_bot')))
  return super().route(route)

def open_page(browser,f,lang,path='/telegram',clock=False):
 page=browser.new_page(viewport=dict(width=390 if lang=='zh' else 1440,height=1000))
 page.set_default_timeout(10000)
 if clock:page.clock.install()
 errors=[];page.on('pageerror',lambda e:errors.append(str(e)))
 page.add_init_script("localStorage.setItem('polaris.token','fixture');localStorage.setItem('polaris.lang',%s);localStorage.setItem('polaris.refresh','0')"%json.dumps(lang))
 page.route('**/api/v1/**',f.route);page.goto(BASE+path)
 return page,errors

def telegram(browser,lang):
 f=Fixture();f.hold_runtime=True
 if lang=='en':f.settings.update(tg_enabled=True,tg_daily_report=False,remind_usage_pct=0,tg_daily_report_hour=0)
 page,errors=open_page(browser,f,lang,clock=True)
 switch=page.get_by_role('switch',name='启用' if lang=='zh' else 'Enabled',exact=True)
 expect(switch).to_have_count(0)
 page.wait_for_function('document.querySelectorAll("[aria-busy=true]").length>0')
 f.release();expect(switch).to_have_attribute('aria-checked','true')
 page.clock.run_for(15001)
 assert 1<=f.runtime_calls<=2,('slow runtime reads were repeatedly replaced',f.runtime_calls)  # StrictMode mounts effects twice.
 expect(page.get_by_text('获取状态中…' if lang=='zh' else 'Fetching status…',exact=True)).to_be_visible()
 f.hold_runtime=False
 for route in f.pending_runtime:f.status(route)
 f.pending_runtime=[]
 expect(page.get_by_text('未运行' if lang=='zh' else 'Not running',exact=True)).to_be_visible()
 expect(switch).to_have_attribute('aria-checked','true')
 expect(page.get_by_role('switch',name='用户提醒' if lang=='zh' else 'User reminders',exact=True)).to_have_attribute('aria-checked','false')
 switch.click();expect(switch).to_have_attribute('aria-checked','false')
 f.runtime=True;page.clock.run_for(5001)
 expect(page.get_by_text('● 运行中 @fixture_bot' if lang=='zh' else '● Running @fixture_bot',exact=True)).to_be_visible()
 expect(switch).to_have_attribute('aria-checked','false')
 save=page.locator('.settings-savebar').get_by_role('button',name='保存' if lang=='zh' else 'Save',exact=True)
 f.reject_write=True;save.click()
 expect(page.locator('.settings-savebar [role=alert]')).to_have_text('Synthetic save failure')
 expect(switch).to_have_attribute('aria-checked','false')
 f.reject_write=False;f.hold_write=True
 save.evaluate('(button)=>{button.click();button.click()}')
 expect(switch).to_be_disabled()
 assert len(f.writes)==2,f.writes
 assert f.writes[-1]['tg_enabled']=='false' and f.writes[-1]['remind_usage_pct']=='0' and f.writes[-1]['tg_daily_report_hour']=='0',f.writes
 f.read_error=True
 for route in f.pending_write:f.commit(route)
 f.pending_write=[];f.hold_write=False
 expect(page.get_by_text('无法读取设置，请重试。' if lang=='zh' else 'Unable to load settings. Please retry.',exact=True)).to_be_visible()
 expect(switch).to_have_count(0)
 f.read_error=False;page.get_by_role('button',name='重试' if lang=='zh' else 'Retry',exact=True).click()
 expect(switch).to_have_attribute('aria-checked','false')
 assert f.settings['tg_token_set'] is True
 f.runtime_error=True
 page.get_by_role('button',name='刷新状态' if lang=='zh' else 'Refresh status',exact=True).click()
 expect(page.get_by_text('状态获取失败' if lang=='zh' else 'Status unavailable',exact=True)).to_be_visible()
 expect(switch).to_have_attribute('aria-checked','false')
 assert not errors,errors
 page.close()

def capabilities(browser):
 f=Fixture();f.settings.update(acme_ready=True,acme_http_ready=False)
 page,errors=open_page(browser,f,'zh','/certificates')
 add=page.get_by_role('button',name='+ 证书',exact=True)
 expect(add).to_be_disabled()
 f.read_error=True;f.release()
 retry=page.get_by_role('button',name='重试',exact=True)
 expect(retry).to_be_visible();expect(add).to_be_disabled()
 f.read_error=False;retry.click();expect(add).to_be_enabled();add.click()
 expect(page.get_by_role('dialog').locator('.seg').first.locator('.on')).to_have_text('ACME')
 assert not f.writes and not errors,(f.writes,errors)
 page.close()
 f=Fixture();f.settings.update(cdn_cf_token_set=True,acme_cf_token_set=False)
 page,errors=open_page(browser,f,'zh','/chains')
 page.get_by_role('tab',name='CDN 配置档',exact=True).click()
 expect(page.get_by_text('未配置：填一次，所有 CDN 配置档共用',exact=True)).to_have_count(0)
 f.read_error=True;f.release()
 retry=page.get_by_role('button',name='重试',exact=True)
 expect(retry).to_be_visible()
 f.read_error=False;retry.click()
 expect(page.get_by_text('已配置：所有 CDN 配置档共用',exact=True)).to_be_visible()
 assert not f.writes and not errors,(f.writes,errors)
 page.close()

# Each form must hide unknown switches, expose a failed read, and recover.
cases=[('/telegram','启用',None),('/tickets','开启工单','工单设置'),('/shop?tab=pay','开放购买',None),('/sub-templates','启用订阅',None),('/convert','用户订阅','转换设置'),('/domains','设为节点域名',None),('/audit','变更记录',None),('/settings#mail','给用户发提醒邮件',None),('/settings#backup','Telegram',None),('/cores','Geodata 自动更新',None),('/probe','延迟探测','设置')]
selected={path for path in os.environ.get('POLARIS_SETTINGS_STATE_CASES','').split(',') if path}
with sync_playwright() as p:
 for engine in ['chromium','webkit']:
  options=dict(executable_path=os.environ.get('POLARIS_CHROMIUM_BIN') or shutil.which('chromium'),args=['--no-sandbox','--disable-dev-shm-usage']) if engine=='chromium' else {}
  browser=getattr(p,engine).launch(**options)
  for lang in ([] if selected else ['zh','en']):
   telegram(browser,lang);print(engine,lang,'Telegram loading, slow status, polling, drafts, failures and readback passed',flush=True)
  for path,label,opener in cases:
   if selected and path not in selected:continue
   f=Fixture()
   if path=='/telegram':f.settings.update(tg_enabled=True,tg_token_set=False)
   page,errors=open_page(browser,f,'zh',path)
   if opener:page.get_by_role('button' if opener!='转换设置' else 'tab',name=opener,exact=True).click()
   control=page.get_by_role('switch',name=label,exact=True)
   expect(control).to_have_count(0)
   f.read_error=True;f.release()
   retry=page.get_by_role('button',name='重试',exact=True).first
   expect(retry).to_be_visible();expect(control).to_have_count(0)
   f.read_error=False;f.malformed=True
   with page.expect_response(lambda r:urlparse(r.url).path=='/api/v1/settings' and r.request.method=='GET') as response:retry.click()
   assert response.value.json()==[]
   if path=='/audit':expect(page.get_by_role('alert')).to_contain_text('无法读取设置，请重试。')
   else:expect(page.get_by_text('设置响应格式无效，请重试。',exact=True).first).to_be_visible()
   expect(control).to_have_count(0)
   f.malformed=False;retry.click()
   if path=='/settings#backup':page.locator('summary').filter(has_text='推送到其他位置').click()
   expect(control.first).to_be_visible()
   if path=='/telegram':
    # The templates panel made its own initial read; recover that too.
    for retry_other in page.get_by_role('button',name='重试',exact=True).all():retry_other.click()
    expect(page.get_by_text('在 @BotFather 创建机器人获取；加密存储',exact=True)).to_be_visible()
   expect(control.first).to_be_visible()
   if path=='/settings#mail':expect(control).to_have_attribute('aria-checked','false')
   if path=='/sub-templates':expect(page.get_by_role('switch',name='机器人绑定',exact=True)).to_have_attribute('aria-checked','false')
   assert not f.writes,f.writes;assert not errors,(path,errors)
   page.close();print(engine,path,'unknown/failed/malformed reads and retry passed',flush=True)
  if not selected or 'metadata' in selected:
   capabilities(browser);print(engine,'certificate and CDN credential readiness passed',flush=True)
  browser.close()
