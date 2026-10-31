"""Node fleets: bounded rendering, explicit selection and browser navigation."""
import copy, json, os, shutil
from pathlib import Path
from urllib.parse import urlparse
from time import monotonic
from playwright.sync_api import sync_playwright, expect

scope={'__file__':str(Path(__file__).with_name('operations-ui-smoke.py'))}
exec(Path(scope['__file__']).read_text().split('with sync_playwright() as p:')[0],scope)
Base=scope['Fixture'];template=scope['node']
BASE=os.environ.get('POLARIS_WEB_TEST_URL','http://127.0.0.1:5187').rstrip('/')
OUT=Path(os.environ.get('POLARIS_NODES_ARTIFACTS','/tmp/polaris-nodes-ui'));OUT.mkdir(parents=True,exist_ok=True)

class Fixture(Base):
 def __init__(self,count=97):
  super().__init__();self.nodes=[]
  for i in range(1,count+1):
   n=copy.deepcopy(template);state=['online','online','offline','syncing','error','pending'][(i-1)%6];region=['HK','US','SG'][(i-1)%3]
   n.update(id=f'n{i}',name=f'{region} Edge {i:03}',region=region,flag={'HK':'🇭🇰','US':'🇺🇸','SG':'🇸🇬'}[region],status=state,connected=state in ['online','syncing','error'],address=f'203.0.113.{i}',publicIpv4=f'203.0.113.{i}',publicIpv6=f'2001:db8::{i}',domain=f'edge-{i}.example.com',labels=['blue'] if i%7==0 else [],lastError='Apply failed' if state=='error' else '',netRx=i*1024**3,netTx=0,netLimit=100*1024**3,netMode='sum',netWarnPct=80)
   n['system'].update(cpu=i-1,mem=i-1,rxBps=i*1000,txBps=i*2000)
   n['netHealth']=dict(state='healthy' if n['connected'] else 'stale',mode='boot',checkedAt=scope['now'],lastSampleAt=scope['now'],staleAfterSeconds=120,lastGapSeconds=0)
   if i==12:n['name']='SG 长名称节点 / Singapore regional edge with a long descriptive name'
   n['cores'][0].update(id=f'c{i}',nodeId=n['id'],type='singbox' if i%2==0 else 'xray')
   self.nodes.append(n)
 def route(self,route):
  path=urlparse(route.request.url).path.removeprefix('/api/v1')
  if path=='/nodes':return route.fulfill(json=self.nodes)
  return super().route(route)

def open_page(browser,fixture,width,lang,path='/nodes'):
 page=browser.new_page(viewport=dict(width=width,height=1000),locale='zh-CN' if lang=='zh' else 'en-US')
 page.set_default_timeout(10000)
 page.add_init_script("localStorage.setItem('polaris.token','fixture');localStorage.setItem('polaris.lang',%s);localStorage.setItem('polaris.refresh','0')"%json.dumps(lang))
 errors=[];page.on('pageerror',lambda e,errors=errors:errors.append(str(e)))
 pending=set();changed=[monotonic()]
 def update(req,start):
  if '/api/v1/' not in req.url:return
  if start:pending.add(req)
  else:pending.discard(req)
  changed[0]=monotonic()
 page.on('request',lambda r:update(r,True));page.on('requestfinished',lambda r:update(r,False));page.on('requestfailed',lambda r:update(r,False))
 def settle():
  changed[0]=monotonic();deadline=changed[0]+10
  while pending or monotonic()-changed[0]<0.5:
   assert monotonic()<deadline,('API did not settle',[r.url for r in pending])
   page.wait_for_timeout(50)
 page.route('**/api/v1/**',fixture.route);page.goto(BASE+path)
 return page,errors,settle

with sync_playwright() as p:
 for engine in ['chromium','webkit']:
  options=dict(executable_path=os.environ.get('POLARIS_CHROMIUM_BIN') or shutil.which('chromium'),args=['--no-sandbox','--disable-dev-shm-usage']) if engine=='chromium' else {}
  browser=getattr(p,engine).launch(**options)
  for width in [390,1440]:
   for lang in ['zh','en']:
    zh=lang=='zh';f=Fixture();page,errors,settle=open_page(browser,f,width,lang)
    fleet=page.get_by_role('region',name='节点浏览' if zh else 'Node browser')
    rows=fleet.locator('tbody tr');expect(rows).to_have_count(12)
    expect(fleet.get_by_role('button',name='列表' if zh else 'List',exact=True)).to_have_attribute('aria-pressed','true')
    assert page.evaluate('document.documentElement.scrollWidth<=innerWidth+1')
    assert fleet.locator('.node-list .scroll').evaluate('(e)=>e.clientHeight<=470')
    page.screenshot(path=str(OUT/f'{engine}-{width}-{lang}-fleet.png'))
    pager=fleet.get_by_role('navigation',name='节点分页' if zh else 'Node pagination')
    pager.get_by_role('button',name='下一页' if zh else 'Next',exact=True).click()
    expect(rows.first).to_have_attribute('data-node-id','n13')
    query=fleet.get_by_role('searchbox',name='搜索节点' if zh else 'Search nodes')
    query.fill('203.0.113.83');expect(rows).to_have_count(1);expect(rows.first).to_have_attribute('data-node-id','n83')
    expect(pager.get_by_role('button',name='上一页' if zh else 'Previous',exact=True)).to_be_disabled()
    # Address/labels search does not silently replace the current action target.
    expect(page.locator('#node-detail-title')).to_have_text('🇭🇰 HK Edge 001')
    choice=fleet.get_by_role('button',name='查看节点 US Edge 083' if zh else 'View node US Edge 083',exact=True)
    choice.focus();page.keyboard.press('Enter')
    expect(page.locator('#node-detail-title')).to_have_text('🇺🇸 US Edge 083')
    expect(page.locator('.node-selected-net')).to_be_visible()
    expect(page.locator('.node-selected-net')).to_contain_text('100 GB')
    expect(page).to_have_url(BASE+'/nodes?id=n83')
    expect(fleet.locator('#node-browser-content')).to_be_hidden()
    expect(page.locator('#node-detail-title')).to_be_focused()
    page.screenshot(path=str(OUT/f'{engine}-{width}-{lang}-selected.png'))
    page.locator('.node-detail-heading').get_by_role('button',name='切换节点' if zh else 'Switch node',exact=True).click();expect(query).to_have_value('203.0.113.83');expect(query).to_be_focused()
    query.fill('no-such-node');expect(rows).to_have_count(0)
    expect(fleet).to_contain_text('当前节点不在筛选结果中' if zh else 'Current node is outside these filters')
    page.get_by_role('button',name='强制同步' if zh else 'Force sync',exact=True).click()
    assert f.last('/nodes/n83/sync')[0]=='POST'
    fleet.get_by_role('button',name='清除筛选' if zh else 'Clear filters',exact=True).first.click();expect(rows).to_have_count(12)
    query.fill('blue');expect(fleet).to_contain_text('显示 1–12 / 13 个节点' if zh else 'Showing 1–12 of 13 nodes')
    query.fill('')
    fleet.get_by_label('节点状态' if zh else 'Node status',exact=True).select_option('offline')
    fleet.get_by_label('节点地区' if zh else 'Node region',exact=True).select_option('SG')
    fleet.get_by_label('节点内核' if zh else 'Node core',exact=True).select_option('xray')
    expect(rows).to_have_count(12)
    assert all('—' in text and '%' not in text for text in rows.locator('.node-resources').all_inner_texts())
    fleet.get_by_role('button',name='清除筛选' if zh else 'Clear filters',exact=True).first.click()
    fleet.get_by_label('节点排序' if zh else 'Node order',exact=True).select_option('cpu')
    expect(rows.first).to_have_attribute('data-node-id','n97')
    # View choice persists, while selection lives in the URL and follows Back.
    fleet.get_by_role('button',name='卡片' if zh else 'Cards',exact=True).click();expect(fleet.locator('.ncard')).to_have_count(12)
    settle();page.reload();expect(fleet.locator('.ncard')).to_have_count(12)
    expect(page.locator('#node-detail-title')).to_have_text('🇺🇸 US Edge 083')
    settle();page.go_back();expect(page.locator('#node-detail-title')).to_have_text('🇭🇰 HK Edge 001')
    settle();page.close();assert not errors,(engine,width,lang,errors)
    # A missing URL selection must not redirect actions to a different node.
    missing,missing_errors,settle_missing=open_page(browser,Fixture(),width,lang,'/nodes?id=gone')
    expect(missing.get_by_text('所选节点已不存在，请重新选择节点' if zh else 'The selected node no longer exists. Select another node.',exact=True)).to_be_visible()
    expect(missing.get_by_role('button',name='强制同步' if zh else 'Force sync',exact=True)).to_have_count(0)
    settle_missing();missing.close();assert not missing_errors,missing_errors
    # Small installations retain the familiar card wall with no stored choice.
    small,small_errors,settle_small=open_page(browser,Fixture(3),width,lang)
    expect(small.locator('.ncard')).to_have_count(3)
    assert small.locator('.node-list').count()==0
    settle_small();small.close();assert not small_errors,small_errors
    # Reordered live data keeps the initial selection; a removed selection
    # exposes no actions until the operator explicitly chooses another node.
    live_fixture=Fixture(30);live,live_errors,settle_live=open_page(browser,live_fixture,width,lang)
    expect(live.locator('#node-detail-title')).to_have_text('🇭🇰 HK Edge 001')
    refresh=live.get_by_role('region',name='网卡流量采样' if zh else 'NIC traffic sampling').get_by_role('button',name='刷新' if zh else 'Refresh',exact=True)
    live_fixture.nodes.reverse();refresh.click()
    expect(live.locator('.node-list tbody tr').first).to_have_attribute('data-node-id','n30')
    expect(live.locator('#node-detail-title')).to_have_text('🇭🇰 HK Edge 001')
    live_fixture.nodes=[n for n in live_fixture.nodes if n['id']!='n1'];refresh.click()
    expect(live.get_by_text('所选节点已不存在，请重新选择节点' if zh else 'The selected node no longer exists. Select another node.',exact=True)).to_be_visible()
    expect(live.get_by_role('button',name='强制同步' if zh else 'Force sync',exact=True)).to_have_count(0)
    settle_live();live.close();assert not live_errors,live_errors
    print(engine,width,lang,'97-node browsing, filters, pagination, selection, URL history and small fleets OK',flush=True)
  browser.close()
