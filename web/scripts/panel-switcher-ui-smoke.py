# Browser regression for the workspace-bar panel switcher. Uses mocked API
# responses; this does not replace real multi-panel acceptance tests.
# Prerequisites: Python playwright and Chromium. Start Vite separately, then:
#   POLARIS_WEB_TEST_URL=http://127.0.0.1:5187 python3 web/scripts/panel-switcher-ui-smoke.py
import json, os, shutil
from pathlib import Path
from urllib.parse import urlparse
from playwright.sync_api import sync_playwright, expect

BASE_URL=os.environ.get('POLARIS_WEB_TEST_URL','http://127.0.0.1:5187').rstrip('/')
CHROMIUM=os.environ.get('POLARIS_CHROMIUM_BIN') or shutil.which('chromium')
OUT=Path(os.environ.get('POLARIS_UI_ARTIFACTS','/tmp/polaris-panel-switcher-ui')); OUT.mkdir(exist_ok=True)

def member(i,name,url,health,enabled=True):
 return dict(id=i,name=name,baseUrl=url,scopes=['panel:read'],caPin='',insecure=False,labels=None,enabled=enabled,createdBy='admin',
  snapshot=dict(version='v0.1.262',capabilities=[],users=0,onlineUsers=0,nodes=0,nodesOnline=0,trafficUp=0,trafficDown=0),
  snapshotAt=None,lastOkAt=None,lastError='',failures=0,nextTryAt=None,createdAt='',updatedAt='',stale=False,ageSeconds=0,health=health)

class Fixture:
 def __init__(self,panels):
  self.panels=panels; self.requests=[]
 def route(self,route):
  req=route.request; path=urlparse(req.url).path.removeprefix('/api/v1'); self.requests.append((req.method,path))
  if path=='/auth/me': result=dict(username='admin')
  elif path=='/info': result=dict(title='Polaris',version='1.0',edition='pro')
  elif path=='/nodes': result=[]
  elif path=='/license': result=dict(edition='pro',state=dict(plan='pro',features=[],warnings=[],limits={}),usage={})
  elif path=='/panels': result=dict(panels=self.panels,pro=True,cap=0)
  else: result=[]
  route.fulfill(status=200,content_type='application/json',body=json.dumps(result))

def open_page(browser,width=1440,path='/nodes'):
 f=Fixture(PANELS)
 page=browser.new_page(viewport=dict(width=width,height=900),locale='zh-CN',timezone_id='Asia/Shanghai')
 page.set_default_timeout(10000)
 page.add_init_script("localStorage.setItem('polaris.token','fixture');localStorage.setItem('polaris.lang','zh');localStorage.setItem('polaris.refresh','0')")
 page.route('**/api/v1/**',lambda route: f.route(route))
 errors=[]; page.on('pageerror',lambda e:errors.append(str(e)))
 page.goto(BASE_URL+path)
 return page,f,errors

def no_overflow(page): assert page.evaluate('document.documentElement.scrollWidth <= window.innerWidth+1')

# Two enabled members with different health, plus a disabled one that must
# not appear: a disabled panel is excluded from aggregation and polling, so
# offering it as a switch target would send people to a panel the hub
# itself no longer trusts.
PANELS=[
 member('p1','dmit','https://dmit.example.com','ok'),
 member('p2','hk','https://hk.example.com','failed'),
 member('p3','retired','https://retired.example.com','ok',enabled=False),
]

with sync_playwright() as p:
 browser=p.chromium.launch(executable_path=CHROMIUM,headless=True,args=['--no-sandbox','--disable-dev-shm-usage'])

 # Wide screens: the chip sits in the workspace bar. The sidebar copy exists
 # in the DOM but stays hidden so there is exactly one switcher on screen.
 page,f,errors=open_page(browser,1440)
 expect(page.locator('.bar-switcher')).to_be_visible()
 expect(page.locator('.side-switcher')).to_be_hidden()
 page.locator('.bar-switcher').click()
 items=page.locator('.bar-switcher .panel-switch-item')
 expect(items).to_have_count(2)
 expect(items.nth(0)).to_contain_text('dmit')
 expect(items.nth(0)).to_have_attribute('href','https://dmit.example.com')
 # A member is another origin with its own login: it opens in a new tab
 # instead of replacing the current session.
 expect(items.nth(0)).to_have_attribute('target','_blank')
 expect(items.nth(0)).to_have_attribute('rel','noreferrer')
 # Health is visible before opening, so a failing member does not look
 # equal to a healthy one.
 expect(items.nth(1).locator('.dot.bad')).to_have_count(1)
 expect(items.nth(1)).to_contain_text('hk.example.com')
 # Disabled members are not offered: the hub already excludes them from
 # aggregation and polling.
 assert not any('retired' in text for text in items.all_inner_texts()), items.all_inner_texts()
 page.screenshot(path=str(OUT/'switcher-1440.png'))
 page.keyboard.press('Escape')
 expect(page.locator('.bar-switcher')).not_to_have_attribute('open','')
 no_overflow(page); assert not errors,errors; page.close()
 print('wide layout: bar chip lists enabled members with health, opens in a new tab OK',flush=True)

 # Phones hide the workspace bar entirely (styles.css), so the switcher has
 # to be reachable from the sidebar drawer or phones cannot switch at all.
 page,f,errors=open_page(browser,390)
 expect(page.locator('.bar-switcher')).to_be_hidden()
 page.get_by_role('button',name='打开菜单').click()
 expect(page.locator('.side-switcher')).to_be_visible()
 page.locator('.side-switcher').click()
 side_items=page.locator('.side-switcher .panel-switch-item')
 expect(side_items).to_have_count(2)
 expect(side_items.nth(0)).to_have_attribute('href','https://dmit.example.com')
 expect(side_items.nth(0)).to_have_attribute('target','_blank')
 # The list opens upward: at the bottom of the sidebar there is no room
 # below, and opening downward would cover the account row.
 box_summary=page.locator('.side-switcher-summary').bounding_box()
 box_list=page.locator('.side-switcher-list').bounding_box()
 assert box_list['y'] + box_list['height'] <= box_summary['y'] + 2, (box_summary, box_list)
 assert box_list['y'] >= 0, box_list
 page.screenshot(path=str(OUT/'switcher-390.png'))
 no_overflow(page); assert not errors,errors; page.close()
 print('phone layout: switcher reachable from the sidebar drawer OK',flush=True)

 # A single member is not a choice: no switcher appears, so a plain
 # single-panel install gets no dead menu in its header.
 PANELS[:]=[member('p1','dmit','https://dmit.example.com','ok')]
 page,f,errors=open_page(browser,1440)
 page.wait_for_timeout(1500)
 expect(page.locator('.panel-switcher')).to_have_count(0)
 no_overflow(page); assert not errors,errors; page.close()
 print('single-member install shows no switcher OK',flush=True)
 browser.close()
