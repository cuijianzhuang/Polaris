"""Online IP evidence, readable unknown states, live details and editor safety."""
import copy,json,os,shutil
from pathlib import Path
from urllib.parse import urlparse
from datetime import datetime,timezone,timedelta
from playwright.sync_api import sync_playwright,expect
scope={'__file__':str(Path(__file__).with_name('operations-ui-smoke.py'))}
exec(Path(scope['__file__']).read_text().split('with sync_playwright() as p:')[0],scope)
Base=scope['Fixture'];BASE=os.environ.get('POLARIS_WEB_TEST_URL','http://127.0.0.1:5187').rstrip('/')
OUT=Path(os.environ.get('POLARIS_ONLINE_ARTIFACTS','/tmp/polaris-online-ui'));OUT.mkdir(parents=True,exist_ok=True)
now=datetime.now(timezone.utc).isoformat()

class Fixture(Base):
 def __init__(self):
  super().__init__();self.fail=False;self.detail_calls=0
  user=dict(id='',username='',uuid='00000000-0000-4000-8000-000000000001',password='fixture',trafficLimit=100*1024**3,usedUp=0,usedDown=0,expireAt=None,expireAfterDays=0,ipLimit=5,status='active',subToken='synthetic-token',resetPolicy='none',resetDay=1,onlineAt=now,onlineIps=[],onlineDevices=[],remark='',groupIds=[],nodeIds=['n1'],inboundIds=[],telegramId=0,email='',planId='',createdAt=now,renewDays=0,renewMax=0,renewCount=0,deviceLimit=0)
  self.users={}
  for name,state in [('known','available'),('unknown','unavailable'),('expired','expired'),('proxy','unavailable'),('partial','partial'),('trusted','available'),('offline','none'),('unlimited','available')]:
   self.users[name]=dict(copy.deepcopy(user),id=name,username=name,onlineIpState=state)
  for name in ['known','partial','unlimited']:
   self.users[name].update(onlineIps=['203.0.113.8'],onlineIpCount=1,onlineIpSeenAt=now,onlineDevices=[dict(ip='203.0.113.8',nodes=['Hong Kong','Tokyo'],seen=now)])
  edge=dict(ip='104.16.1.1',nodes=['Hong Kong'],seen=now,excluded='cdn')
  for name in ['proxy','partial']:
   self.users[name]['onlineIps'].append(edge['ip']);self.users[name]['onlineDevices'].append(edge)
  self.users['trusted'].update(onlineIps=['203.0.113.20'],onlineIpCount=0,onlineIpSeenAt=now,onlineDevices=[dict(ip='203.0.113.20',nodes=['Hong Kong'],seen=now,excluded='trusted')])
  self.users['expired']['onlineIpSeenAt']=(datetime.now(timezone.utc)-timedelta(minutes=2)).isoformat()
  self.users['offline']['onlineAt']=(datetime.now(timezone.utc)-timedelta(hours=1)).isoformat()
  self.users['unlimited']['ipLimit']=0
 def route(self,route):
  path=urlparse(route.request.url).path.removeprefix('/api/v1')
  if path=='/users':return route.fulfill(json=dict(items=list(self.users.values()),total=len(self.users),summary={'all':len(self.users),'active':len(self.users),'online':7}))
  if path.startswith('/users/') and path.split('/')[-1] in self.users:
   self.detail_calls+=1
   if self.fail:return route.fulfill(status=503,json={'error':'fixture refresh failed'})
   return route.fulfill(json=self.users[path.split('/')[-1]])
  return super().route(route)

with sync_playwright() as p:
 for engine in ['chromium','webkit']:
  opts=dict(executable_path=os.environ.get('POLARIS_CHROMIUM_BIN') or shutil.which('chromium'),args=['--no-sandbox','--disable-dev-shm-usage']) if engine=='chromium' else {}
  browser=getattr(p,engine).launch(**opts)
  for width in [390,1440]:
   for lang in ['zh','en']:
    zh=lang=='zh';f=Fixture();page=browser.new_page(viewport=dict(width=width,height=1000));page.set_default_timeout(10000);page.clock.install()
    page.add_init_script("localStorage.setItem('polaris.token','fixture');localStorage.setItem('polaris.refresh','0');localStorage.setItem('polaris.lang',%s)"%json.dumps(lang))
    errors=[];page.on('pageerror',lambda e,errors=errors:errors.append(str(e)));page.route('**/api/v1/**',f.route)
    page.goto(BASE+'/users');expect(page.locator('[data-user-id]')).to_have_count(8)
    row=lambda name:page.locator(f'[data-user-id="{name}"]')
    for name,text in [('known','1 / 5'),('trusted','0 / 5'),('partial','≥1 / 5'),('unlimited','1 / ∞'),('offline','0 / 5')]:expect(row(name).locator('.online-ip-count')).to_have_text(text)
    expect(row('unknown').locator('.online-ip-count')).to_have_text(('未识别' if zh else 'Unknown')+' / 5')
    expect(row('expired').locator('.online-ip-count')).to_have_text(('已过期' if zh else 'Expired')+' / 5')
    expect(row('proxy').locator('.online-ip-count')).to_have_text(('未识别' if zh else 'Unknown')+' / 5')
    expect(row('unknown')).to_contain_text('近期活跃' if zh else 'Recently active')
    assert page.evaluate('document.documentElement.scrollWidth<=innerWidth+1')
    page.screenshot(path=str(OUT/f'{engine}-{width}-{lang}-list.png'))
    row('proxy').locator('.online-ip-count').click()
    dialog=page.get_by_role('dialog');expect(dialog).to_contain_text('104.16.1.1');expect(dialog).to_contain_text('CDN 边缘，不计入' if zh else 'CDN edge, excluded')
    page.keyboard.press('Escape');page.clock.run_for(200);expect(dialog).to_have_count(0)
    row('known').locator('.online-ip-count').click();expect(dialog).to_contain_text('203.0.113.8');expect(dialog).to_contain_text('Hong Kong · Tokyo')
    # Detail refreshes independently from the paused main-list refresh.
    f.users['known']['onlineDevices'].append(dict(ip='2001:db8::1234',nodes=['Tokyo'],seen=now))
    f.users['known']['onlineIps'].append('2001:db8::1234');f.users['known']['onlineIpCount']=2
    page.clock.run_for(10010);expect(dialog).to_contain_text('2001:db8::1234');assert f.detail_calls>=3
    f.fail=True;page.clock.run_for(10010);expect(dialog.get_by_role('alert')).to_contain_text('fixture refresh failed');expect(dialog).to_contain_text('2001:db8::1234')
    f.fail=False;dialog.get_by_role('button',name='重试' if zh else 'Retry',exact=True).click();expect(dialog.get_by_role('alert')).to_have_count(0)
    page.screenshot(path=str(OUT/f'{engine}-{width}-{lang}-detail.png'))
    page.keyboard.press('Escape');page.clock.run_for(200);expect(dialog).to_have_count(0)
    row('known').get_by_role('button',name='编辑' if zh else 'Edit',exact=True).click()
    name=page.locator('#u-name');expect(name).to_have_value('known');name.fill('unsaved-name')
    f.users['known']['username']='server-name';page.clock.run_for(10010)
    expect(name).to_have_value('unsaved-name')
    expect(page.locator('.drawer .online-user-details')).to_contain_text('2001:db8::1234')
    assert not errors,(engine,width,lang,errors)
    page.close();print(engine,width,lang,'IP availability, quota counts, proxy sources, polling/error recovery and unsaved editor OK',flush=True)
  browser.close()
